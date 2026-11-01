//
//  Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//

// EPUBParser extracts text content from EPUB files. EPUB is a ZIP
// container with XHTML spine items. The parser:
//
//  1. Opens the EPUB as a ZIP archive.
//  2. Reads META-INF/container.xml to locate the OPF manifest.
//  3. Walks the OPF spine to collect content documents in order.
//  4. Emits text, tables and image references from each chapter in document order.
//
// Chapter images are read from their ZIP entries and written to the run media
// cache; the result carries only a MediaRef. Inline data URIs take the same
// ref-only path as the HTML parser.
//
// The output format is "json", matching the epub default in
// ParserParam.Defaults().

package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"ragflow/internal/entity"
	"ragflow/internal/parser/charset"
	"ragflow/internal/parser/media"
)

// EPUBParser extracts text from EPUB archives.
type EPUBParser struct {
	textOnly bool
}

func NewEPUBParser() *EPUBParser {
	return &EPUBParser{}
}

// NewEPUBTextParser extracts EPUB text without reading or caching images.
// Callers without a run media cache use it to keep the previous text-only
// behavior.
func NewEPUBTextParser() *EPUBParser {
	return &EPUBParser{textOnly: true}
}

func (p *EPUBParser) String() string {
	return "EPUBParser"
}

// ParseWithResult implements ParseResultProducer. It extracts XHTML
// content from the EPUB spine using the shared HTML structure walker.
func (p *EPUBParser) ParseWithResult(ctx context.Context, filename string, data []byte) ParseResult {
	if len(data) == 0 {
		return ParseResult{
			File: map[string]any{
				"name":     filename,
				"size":     0,
				"encoding": "utf-8",
			},
			JSON: []DocItem{NewTextJSONItem("")},
		}
	}

	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ParseResult{Err: fmt.Errorf("epub: open zip: %w", err)}
	}

	opfPath, err := readContainerXML(reader)
	if err != nil {
		return ParseResult{Err: fmt.Errorf("epub: container.xml: %w", err)}
	}

	opf, err := readOPFManifest(reader, opfPath)
	if err != nil {
		return ParseResult{Err: fmt.Errorf("epub: opf: %w", err)}
	}

	navPoints, navWarnings := readEPUBNavPoints(reader, opf)
	items, detectedEnc, warnings, err := extractEPUBTextItems(ctx, reader, opf, p.textOnly, navPoints)
	if err != nil {
		return ParseResult{Err: fmt.Errorf("epub: %w", err)}
	}
	warnings = append(navWarnings, warnings...)
	if items == nil {
		items = []map[string]any{{"text": "", "doc_type_kwd": "text"}}
	}
	return ParseResult{
		File: map[string]any{
			"name":     filename,
			"size":     len(data),
			"encoding": detectedEnc,
		},
		JSON:     MapsToItems(items),
		Warnings: warnings,
	}
}

// --- container.xml parsing ---

type containerXML struct {
	RootFiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

func readContainerXML(r *zip.Reader) (string, error) {
	f, err := r.Open("META-INF/container.xml")
	if err != nil {
		return "", fmt.Errorf("open container.xml: %w", err)
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read container.xml: %w", err)
	}
	var c containerXML
	if err := xml.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	if len(c.RootFiles) == 0 {
		return "", fmt.Errorf("no rootfile in container.xml")
	}
	return c.RootFiles[0].FullPath, nil
}

// --- OPF parsing ---

type opfPackage struct {
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			Type       string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		ItemRefs []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

// opfManifest is the parsed OPF package document.
type opfManifest struct {
	// Dir is the ZIP directory that contains the package document. Spine and
	// manifest hrefs resolve against it.
	Dir string
	// SpineHrefs lists content-document hrefs in reading order.
	SpineHrefs []string
	// SpineEntries holds the ZIP entry paths the spine can open, both the
	// OPF-relative resolved names and the raw manifest-href aliases. Nav
	// links are only usable when they resolve into this set.
	SpineEntries map[string]struct{}
	// NavHref is the resolved ZIP entry of the EPUB 3 navigation document
	// (the manifest item carrying properties="nav"). Empty for books that
	// declare none (EPUB 2 or no table of contents).
	NavHref string
	// Images maps a resolved ZIP entry path to the media type the package
	// declares for it.
	Images map[string]string
}

func readOPFManifest(r *zip.Reader, opfPath string) (*opfManifest, error) {
	raw, dir, err := readOPFBytes(r, opfPath)
	if err != nil {
		return nil, err
	}
	var pkg opfPackage
	if err := xml.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("parse opf: %w", err)
	}

	// Build id → href lookup.
	byID := make(map[string]string, len(pkg.Manifest.Items))
	images := make(map[string]string)
	var navHref string
	for _, it := range pkg.Manifest.Items {
		byID[it.ID] = it.Href
		mime := strings.ToLower(strings.TrimSpace(it.Type))
		if strings.HasPrefix(mime, "image/") {
			if entry, ok := resolveEPUBEntry(dir, it.Href); ok {
				images[entry] = mime
			}
			continue
		}
		// The EPUB 3 navigation document is the manifest item whose
		// properties token list contains "nav".
		if navHref == "" && epubHasToken(it.Properties, "nav") {
			if entry, ok := resolveEPUBEntry(dir, it.Href); ok {
				navHref = entry
			}
		}
	}

	// Build spine item list (ordered hrefs) together with every ZIP entry
	// name its href can open.
	spineEntries := make(map[string]struct{})
	var hrefs []string
	for _, ref := range pkg.Spine.ItemRefs {
		href, ok := byID[ref.IDRef]
		if !ok {
			continue
		}
		hrefs = append(hrefs, href)
		if entry, ok := resolveEPUBEntry(dir, href); ok {
			spineEntries[entry] = struct{}{}
		}
		if bare := epubBareHref(href); bare != "" {
			spineEntries[bare] = struct{}{}
		}
	}
	return &opfManifest{Dir: dir, SpineHrefs: hrefs, SpineEntries: spineEntries, NavHref: navHref, Images: images}, nil
}

// epubBareHref decodes a manifest href relative to the archive root without
// joining it against the OPF directory. readEPUBContentFile falls back to
// this raw name, so spine and nav matching accept it too.
func epubBareHref(href string) string {
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	unescaped, err := url.PathUnescape(strings.TrimSpace(href))
	if err != nil {
		return ""
	}
	if unescaped == "" || strings.HasPrefix(unescaped, "/") {
		return ""
	}
	if u, err := url.Parse(unescaped); err == nil && u.Scheme != "" {
		return ""
	}
	return unescaped
}

// epubHasToken reports whether the whitespace-separated attribute value
// carries token (case-insensitive), matching the IDPF token-list rules used
// by the manifest properties and epub:type attributes.
func epubHasToken(value, token string) bool {
	for _, part := range strings.FieldsFunc(value, isCollapsibleWS) {
		if strings.EqualFold(part, token) {
			return true
		}
	}
	return false
}

// readOPFBytes reads the package document and returns its ZIP directory. Some
// EPUBs declare the OPF path with a leading slash, so the trimmed name is
// tried as well.
func readOPFBytes(r *zip.Reader, opfPath string) ([]byte, string, error) {
	names := []string{strings.TrimSpace(opfPath)}
	if trimmed := strings.TrimPrefix(names[0], "/"); trimmed != names[0] {
		names = append(names, trimmed)
	}
	var lastErr error
	for _, name := range names {
		f, err := r.Open(name)
		if err != nil {
			lastErr = err
			continue
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read opf: %w", err)
		}
		return raw, path.Dir(name), nil
	}
	return nil, "", fmt.Errorf("open opf %q: %w", opfPath, lastErr)
}

// resolveEPUBEntry resolves an EPUB href or image source to a ZIP entry path.
// baseDir is the ZIP directory of the document that references it. Remote
// URLs, absolute paths, and references that escape the archive root are
// rejected.
func resolveEPUBEntry(baseDir, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	if i := strings.IndexByte(ref, '#'); i >= 0 {
		ref = ref[:i]
	}
	unescaped, err := url.PathUnescape(ref)
	if err != nil {
		return "", false
	}
	ref = unescaped
	if ref == "" || strings.HasPrefix(ref, "/") {
		return "", false
	}
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" {
		return "", false
	}
	resolved := path.Join(baseDir, ref)
	if resolved == "" || resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", false
	}
	return resolved, true
}

// --- text extraction ---

var epubSpaceEntity = regexp.MustCompile(`&(?:nbsp|#0*160|#[xX]0*[aA]0);`)

// epubSelfClosingRawText matches a self-closing non-void XHTML element whose
// content HTML parsers treat as raw text. HTML5 parsing turns <script src="x"/>
// into an unclosed start tag, so the rest of the chapter becomes script text
// and the walker drops it. Restoring the end tag keeps the chapter body.
var epubSelfClosingRawText = regexp.MustCompile(`(?is)<(script|style|title|textarea)(\s[^<>]*?)?\s*/>`)

func normalizeEPUBSelfClosing(source string) string {
	return epubSelfClosingRawText.ReplaceAllString(source, "<$1$2></$1>")
}

// extractEPUBTextItems reads chapters in spine order and preserves their blocks.
// An item that cannot be read is skipped; when items were
// skipped and no other item could be read, it returns an error instead of an
// empty result. An item that holds no text (an image-only chapter) still counts
// as read, and an empty item counts as neither.
//
// navPoints are EPUB 3 navigation entries resolved against the spine. The
// points never reorder content — body always follows the spine — they only
// insert heading items at document starts and at the in-page anchors the
// links point to. Points whose target document is unreadable or whose
// fragment cannot be matched are reported as warnings and dropped; every
// other point and all readable body text is kept.
func extractEPUBTextItems(ctx context.Context, r *zip.Reader, opf *opfManifest, textOnly bool, navPoints []epubNavPoint) ([]map[string]any, string, []string, error) {
	var items []map[string]any
	var failures []string
	var navWarnings []string
	read := 0
	state := &htmlWalkState{
		refOnly:     true,
		ctx:         ctx,
		mediaCache:  media.CacheFromContext(ctx),
		budget:      media.NewBudget(),
		mediaFormat: "epub",
		skipImages:  textOnly,
	}
	if textOnly {
		// Preserve alt text without resolving or caching image bytes, matching
		// NewHTMLTextParser.
		state.resolveImage = func(string) (string, bool) { return "", false }
	}
	entries := make(map[string]*zip.File, len(r.File))
	for _, f := range r.File {
		// Match zip.Reader.Open, which resolves duplicate names to the first
		// entry.
		if _, ok := entries[f.Name]; !ok {
			entries[f.Name] = f
		}
	}
	// Index nav points by every entry name their target can open; a single
	// point is listed once but may be keyed under both the OPF-resolved and
	// raw manifest-href names.
	pointsByEntry := make(map[string][]epubNavPoint)
	for _, p := range navPoints {
		pointsByEntry[p.entry] = append(pointsByEntry[p.entry], p)
	}
	located := make(map[int]bool)
	rejected := make(map[int]bool)
	detectedEncoding := "utf-8"
	for _, href := range opf.SpineHrefs {
		if state.err != nil {
			break
		}
		raw, chapterEntry, err := readEPUBContentFile(r, opf.Dir, href)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", href, err))
			continue
		}
		if len(raw) == 0 {
			continue
		}
		read++
		decoded, enc := charset.DecodeToUTF8(raw, "application/xhtml+xml")
		if detectedEncoding == "utf-8" && enc != "" && enc != "utf-8" {
			detectedEncoding = enc
		}
		// Retain chapter text normalization for named quotes and spaces.
		source := epubSpaceEntity.ReplaceAllString(string(decoded), " ")
		source = strings.NewReplacer("&lsquo;", "'", "&rsquo;", "'", "&ldquo;", "\"", "&rdquo;", "\"").Replace(source)
		source = normalizeEPUBSelfClosing(source)
		doc, err := html.Parse(strings.NewReader(source))
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", href, err))
			continue
		}
		// When the navigation document is itself a spine item, its TOC link
		// list is navigation chrome, not body; drop that one nav subtree but
		// keep the rest of the document.
		if chapterEntry == opf.NavHref {
			if toc := findEPUBTOCNav(doc); toc != nil {
				detachHTMLNode(toc)
			}
		}
		points := pointsByEntry[chapterEntry]
		if len(points) == 0 {
			if resolved := epubBareHref(href); resolved != "" && resolved != chapterEntry {
				points = pointsByEntry[resolved]
			}
		}
		docStart, at := epubChapterHeadings(doc, points, href, &navWarnings, located, rejected)
		// Image sources are relative to the chapter, not to the OPF.
		state.resolveMedia = epubMediaResolver(entries, state, chapterEntry, opf.Images)
		items = appendEPUBChapter(items, doc, state, docStart, at)
	}
	for _, p := range navPoints {
		if located[p.order] || rejected[p.order] {
			continue
		}
		navWarnings = append(navWarnings, fmt.Sprintf("epub navigation link %q skipped: target document could not be read", p.href))
	}
	if state.err != nil {
		return nil, detectedEncoding, append(navWarnings, state.warnings()...), state.err
	}
	if len(failures) > 0 && read == 0 {
		return nil, detectedEncoding, append(navWarnings, state.warnings()...), fmt.Errorf("no readable content: %d of %d content items could not be read (%s)", len(failures), len(opf.SpineHrefs), failures[0])
	}
	return items, detectedEncoding, append(navWarnings, state.warnings()...), nil
}

// epubMediaResolver returns the image resolver for one chapter. src is an image
// reference relative to the chapter's ZIP entry. The entry bytes are stored in
// the run media cache and the caller receives only the reference.
//
// A source that cannot be mapped to a readable manifest image is skipped and
// counted in the shared warning. Cache failures fail the task; a run media
// limit skips the image with a warning instead.
func epubMediaResolver(entries map[string]*zip.File, state *htmlWalkState, chapterEntry string, images map[string]string) func(string) (*entity.MediaRef, bool) {
	chapterDir := path.Dir(chapterEntry)
	return func(src string) (*entity.MediaRef, bool) {
		if state.err != nil {
			return nil, false
		}
		// Inline data URIs keep the shared data-URI path.
		if media.IsDataImageSource(src) {
			return nil, false
		}
		if state.mediaCache == nil {
			state.err = fmt.Errorf("%s media cache is not configured", state.cacheFormat())
			return nil, false
		}
		name, ok := resolveEPUBEntry(chapterDir, src)
		if !ok {
			state.unresolvedImages++
			return nil, false
		}
		// The memo is shared by every chapter: one ZIP entry is read, hashed,
		// and cached once per parse, even when different chapters reference it.
		// A nil value records a source that was already rejected, so repeated
		// references do not re-read it or inflate the warning counters.
		if ref, seen := state.mediaOutput().Lookup("entry:" + name); seen {
			if ref == nil {
				return nil, false
			}
			return ref, true
		}
		mime, ok := images[name]
		if !ok {
			state.unresolvedImages++
			state.mediaOutput().Skip("entry:" + name)
			return nil, false
		}
		entry, ok := entries[name]
		if !ok {
			state.unresolvedImages++
			state.mediaOutput().Skip("entry:" + name)
			return nil, false
		}
		return state.loadEPUBMedia(name, mime, entry)
	}
}

// loadEPUBMedia applies size admission, reads one ZIP entry, and writes it to
// the run media cache. It memoizes both success and failure so a source shared
// by multiple chapters performs the work and records warnings once.
func (s *htmlWalkState) loadEPUBMedia(name, mime string, entry *zip.File) (*entity.MediaRef, bool) {
	maxBytes := media.ImageMaxBytes()
	declared := media.Size(maxBytes + 1)
	if entry.UncompressedSize64 <= uint64(maxBytes) {
		declared = media.Size(entry.UncompressedSize64)
	}
	ref, err := s.mediaOutput().Resolve("entry:"+name, declared, func() (string, []byte, bool) {
		f, err := entry.Open()
		if err != nil {
			s.unresolvedImages++
			return "", nil, false
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
		if err != nil || len(data) == 0 {
			s.unresolvedImages++
			return "", nil, false
		}
		return mime, data, true
	})
	s.err = err
	return ref, ref != nil
}

// epubNavHeading is one navigation title to emit at a fixed position inside a
// chapter. The level is the 1-based nesting depth of the entry in the EPUB 3
// navigation document.
type epubNavHeading struct {
	level int
	text  string
}

// appendEPUBChapter keeps continuous chapter prose together, splitting only
// at tables, images, and navigation headings. The shared leaf walker
// preserves their source order.
//
// docStart headings emit before any chapter content; at maps parsed anchor
// elements to the headings whose nav links point at them. Heading items
// carry layout "heading" and an ATX marker ("#"*level) so the existing
// hierarchical title chunker reconstructs the nav parent-child levels.
//
// All prose shares one leafWriter for the chapter, exactly as the previous
// single-pass render did: block boundaries fold to whitespace and unusual
// spacing (e.g. U+3000) survives until a heading, table, or image forces a
// real item boundary.
func appendEPUBChapter(items []map[string]any, doc *html.Node, state *htmlWalkState, docStart []epubNavHeading, at map[*html.Node][]epubNavHeading) []map[string]any {
	var chapter []map[string]any
	w := &leafWriter{b: &bytes.Buffer{}}
	flush := func() {
		flushLeafText(w, &chapter, "text", true)
	}
	emitHeading := func(h epubNavHeading) {
		flush()
		appendEPUBNavHeading(&chapter, h)
	}
	emitHeadings := func(hs []epubNavHeading) {
		for _, h := range hs {
			emitHeading(h)
		}
	}

	// visit renders one block-level element. Headings anchored on the node
	// itself split prose first; headings anchored inside a node rendered
	// atomically (paragraphs, list items, tables, …) split right before its
	// block so a title never lands inside table/image content.
	var visit func(n *html.Node)
	visitChildren := func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if state.err != nil {
				return
			}
			if c.Type == html.CommentNode {
				continue
			}
			if c.Type == html.ElementNode && epubSkippedTag(c.Data) {
				continue
			}
			if c.Type == html.ElementNode && (isBlockTag(c.Data) || c.Data == "table" || isEPUBContainer(c.Data)) {
				visit(c)
				continue
			}
			// Text nodes and phrasing elements fold into the shared prose
			// writer. A heading anchored inside an inline subtree splits the
			// run at that element.
			if c.Type == html.ElementNode {
				emitHeadings(epubHeadingsWithin(c, at))
			}
			walkHTMLLeaf(c, w, &chapter, "text", true, state)
		}
	}
	visit = func(n *html.Node) {
		blockLike := isBlockTag(n.Data) || n.Data == "table" || isEPUBContainer(n.Data)
		if !w.pre && blockLike && w.b.Len() > 0 && !w.endsNL {
			w.hardBreak()
		}
		emitHeadings(at[n])
		switch {
		case n.Data == "table":
			emitHeadings(epubHeadingsWithin(n, at))
			flush()
			emitHTMLTable(n, &chapter, state)
		case isEPUBContainer(n.Data):
			visitChildren(n)
		default:
			emitHeadings(epubHeadingsWithin(n, at))
			walkHTMLLeaf(n, w, &chapter, "text", true, state)
		}
		// Mirror walkHTMLLeaf's post-block line break so the next sibling
		// block does not run into this one in the shared writer.
		if !w.pre && blockLike && w.b.Len() > 0 && !w.endsNL {
			w.hardBreak()
		}
	}

	for _, h := range docStart {
		appendEPUBNavHeading(&chapter, h)
	}
	for root := doc.FirstChild; root != nil; root = root.NextSibling {
		if root.Type != html.ElementNode || epubSkippedTag(root.Data) {
			continue
		}
		visitChildren(root)
	}
	flush()

	// Fold any consecutive prose items (normally only when no heading,
	// table, or image separated them) with the same whitespace folding the
	// single-pass render applied. Heading and media items stay standalone.
	start := len(items)
	for _, item := range chapter {
		if item["doc_type_kwd"] != "text" {
			items = append(items, item)
			continue
		}
		if item["layout"] == "heading" {
			items = append(items, item)
			continue
		}
		text := strings.Join(strings.FieldsFunc(stringValue(item["text"]), isCollapsibleWS), " ")
		if text == "" {
			continue
		}
		if len(items) > start {
			prev := items[len(items)-1]
			if prev["doc_type_kwd"] == "text" && prev["layout"] != "heading" {
				prev["text"] = stringValue(prev["text"]) + " " + text
				continue
			}
		}
		items = append(items, map[string]any{"text": text, "doc_type_kwd": "text"})
	}
	return items
}

// epubSkippedTag reports elements whose content is invisible in the chapter
// body, matching walkHTMLLeaf's skip set.
func epubSkippedTag(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "template":
		return true
	}
	return false
}

// isEPUBContainer reports block elements the chapter walker descends into so
// nav anchors nested several levels deep still split the body at their
// position. Rendering their children separately instead of as one leaf is
// output-equivalent when no heading fires: appendEPUBChapter rejoins the
// adjacent prose runs with a single space.
func isEPUBContainer(tag string) bool {
	switch tag {
	case "body", "section", "article", "div", "main", "header", "footer",
		"aside", "nav", "blockquote", "ul", "ol", "menu", "dl", "details",
		"dialog", "form", "fieldset", "center", "search", "hgroup", "figure":
		return true
	}
	return false
}

// epubHeadingsWithin collects headings anchored strictly inside n in
// document (depth-first pre-order) order. It serves blocks rendered
// atomically, where the anchor cannot interrupt the block's own rendering.
func epubHeadingsWithin(n *html.Node, at map[*html.Node][]epubNavHeading) []epubNavHeading {
	var found []epubNavHeading
	var walk func(node *html.Node)
	walk = func(node *html.Node) {
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			found = append(found, at[c]...)
			walk(c)
		}
	}
	walk(n)
	return found
}

// appendEPUBNavHeading emits one nav title as a heading item. The ATX marker
// mirrors the Markdown parser, whose "#" count the title chunker maps to
// heading levels; depths beyond the six HTML heading levels cap at six.
func appendEPUBNavHeading(out *[]map[string]any, h epubNavHeading) {
	title := strings.TrimSpace(h.text)
	if title == "" {
		return
	}
	level := h.level
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	*out = append(*out, map[string]any{
		"text":         strings.Repeat("#", level) + " " + title,
		"doc_type_kwd": "text",
		"layout":       "heading",
	})
}

// detachHTMLNode unlinks n from its parent. x/net/html exposes no removal
// API, so the sibling pointers are rewired directly.
func detachHTMLNode(n *html.Node) {
	p := n.Parent
	if p == nil {
		return
	}
	prev, next := n.PrevSibling, n.NextSibling
	if prev != nil {
		prev.NextSibling = next
	} else if p.FirstChild == n {
		p.FirstChild = next
	}
	if next != nil {
		next.PrevSibling = prev
	} else if p.LastChild == n {
		p.LastChild = prev
	}
	n.Parent, n.PrevSibling, n.NextSibling = nil, nil, nil
}

// readEPUBContentFile resolves a spine href (relative to the OPF directory)
// inside the ZIP and returns the item's raw bytes together with the entry name
// it actually opened.
func readEPUBContentFile(r *zip.Reader, opfDir, href string) ([]byte, string, error) {
	// Manifest hrefs are URLs: drop the fragment and decode %-escapes to get
	// the ZIP entry name.
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	if unescaped, err := url.PathUnescape(href); err == nil {
		href = unescaped
	}
	// Resolve href relative to the directory containing the OPF.
	// Use path.Join/Dir (slash separator) because ZIP entry paths are always POSIX.
	resolved := path.Join(opfDir, href)

	var lastErr error
	// Try the resolved path first, then raw href.
	for _, name := range []string{resolved, href} {
		if strings.TrimSpace(name) == "" {
			continue
		}
		f, err := r.Open(name)
		if err != nil {
			lastErr = err
			continue
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, "", err
		}
		return raw, name, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("empty href")
	}
	return nil, "", lastErr
}

// --- EPUB 3 navigation document ---

// epubNavPoint is one locatable table-of-contents entry. It keeps the
// title text, its 1-based nesting depth in the nav <ol> tree, and the
// spine entry plus optional fragment its link points at. Order is the
// point's index in the resolved point list, used to track placement.
type epubNavPoint struct {
	level    int
	title    string
	entry    string
	fragment string
	href     string
	order    int
}

// epubRawNavEntry is one nav list entry before target resolution. A
// header <span> (EPUB 3.0.1) borrows the href of its first descendant
// link so it can still be placed at that section's start.
type epubRawNavEntry struct {
	level int
	title string
	href  string
}

// readEPUBNavPoints reads the EPUB 3 navigation document and resolves its
// TOC links against the spine. A missing nav item is not an error: the
// caller keeps plain spine parsing. An unreadable or empty navigation
// document yields one warning and no points, falling back to plain spine
// parsing; individual links that cannot be located are skipped with a
// warning while every other link is kept.
func readEPUBNavPoints(r *zip.Reader, opf *opfManifest) ([]epubNavPoint, []string) {
	if opf.NavHref == "" {
		return nil, nil
	}
	f, err := r.Open(opf.NavHref)
	if err != nil {
		return nil, []string{fmt.Sprintf("epub navigation document %q could not be read: %v; using spine text without navigation headings", opf.NavHref, err)}
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return nil, []string{fmt.Sprintf("epub navigation document %q could not be read: %v; using spine text without navigation headings", opf.NavHref, err)}
	}
	decoded, _ := charset.DecodeToUTF8(raw, "application/xhtml+xml")
	source := epubSpaceEntity.ReplaceAllString(string(decoded), " ")
	source = strings.NewReplacer("&lsquo;", "'", "&rsquo;", "'", "&ldquo;", "\"", "&rdquo;", "\"").Replace(source)
	source = normalizeEPUBSelfClosing(source)
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil || doc == nil {
		return nil, []string{fmt.Sprintf("epub navigation document %q could not be parsed: %v; using spine text without navigation headings", opf.NavHref, err)}
	}
	toc := findEPUBTOCNav(doc)
	if toc == nil {
		return nil, []string{"epub navigation document contains no table of contents; using spine text without navigation headings"}
	}
	list := epubFindFirstElement(toc, "ol")
	if list == nil {
		return nil, []string{"epub navigation table of contents contains no entries; using spine text without navigation headings"}
	}
	var rawEntries []epubRawNavEntry
	collectEPUBNavList(list, 1, &rawEntries)
	if len(rawEntries) == 0 {
		return nil, []string{"epub navigation table of contents contains no entries; using spine text without navigation headings"}
	}

	var warnings []string
	navDir := path.Dir(opf.NavHref)
	points := make([]epubNavPoint, 0, len(rawEntries))
	for _, e := range rawEntries {
		title := strings.TrimSpace(e.title)
		if title == "" {
			continue
		}
		target, fragment := splitEPUBNavHref(e.href)
		entry := opf.NavHref
		if target != "" {
			resolved, ok := resolveEPUBEntry(navDir, target)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("epub navigation link %q skipped: link target is outside the book", e.href))
				continue
			}
			entry = resolved
		}
		if _, ok := opf.SpineEntries[entry]; !ok {
			warnings = append(warnings, fmt.Sprintf("epub navigation link %q skipped: target is outside the spine reading order", e.href))
			continue
		}
		points = append(points, epubNavPoint{
			level:    e.level,
			title:    title,
			entry:    entry,
			fragment: fragment,
			href:     e.href,
			order:    len(points),
		})
	}
	return points, warnings
}

// splitEPUBNavHref splits a nav href into its percent-decoded target path
// and fragment parts.
func splitEPUBNavHref(href string) (target, fragment string) {
	if i := strings.IndexByte(href, '#'); i >= 0 {
		target, fragment = href[:i], href[i+1:]
	} else {
		target = href
	}
	if decoded, err := url.PathUnescape(target); err == nil {
		target = decoded
	}
	if decoded, err := url.PathUnescape(fragment); err == nil {
		fragment = decoded
	}
	return target, fragment
}

// findEPUBTOCNav returns the navigation element carrying the TOC. EPUB 3
// marks it with epub:type="toc"; role="doc-toc" is accepted as a fallback
// when no epub:type marker is present.
func findEPUBTOCNav(doc *html.Node) *html.Node {
	var roleFallback *html.Node
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "nav" {
			if epubHasToken(htmlAttribute(n, "epub:type"), "toc") {
				found = n
				return
			}
			if roleFallback == nil && strings.EqualFold(htmlAttribute(n, "role"), "doc-toc") {
				roleFallback = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if found != nil {
		return found
	}
	return roleFallback
}

// epubFindFirstElement returns the first depth-first descendant element
// with the given tag.
func epubFindFirstElement(n *html.Node, tag string) *html.Node {
	var found *html.Node
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				if c.Data == tag {
					found = c
					return true
				}
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	walk(n)
	return found
}

// collectEPUBNavList walks one nav <ol>, appending one raw entry per
// <li><a> and recursing into nested lists at depth+1. An <li> headed by a
// <span> instead of an <a> (EPUB 3.0.1) borrows its first descendant
// link's href; a span with no descendant link contributes nothing.
func collectEPUBNavList(ol *html.Node, depth int, out *[]epubRawNavEntry) {
	for li := ol.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.Data != "li" {
			continue
		}
		var link, span *html.Node
		var sublists []*html.Node
		for c := li.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.Data {
			case "a":
				if link == nil {
					link = c
				}
			case "span":
				if span == nil {
					span = c
				}
			case "ol":
				sublists = append(sublists, c)
			}
		}
		switch {
		case link != nil:
			*out = append(*out, epubRawNavEntry{level: depth, title: epubNodeText(link), href: htmlAttribute(link, "href")})
		case span != nil:
			if href, ok := epubFirstDescendantHref(li); ok {
				*out = append(*out, epubRawNavEntry{level: depth, title: epubNodeText(span), href: href})
			}
		}
		if len(sublists) == 0 {
			// Tolerate producers that wrap the nested list in another element.
			if nested := epubFindFirstElement(li, "ol"); nested != nil {
				sublists = []*html.Node{nested}
			}
		}
		for _, sub := range sublists {
			collectEPUBNavList(sub, depth+1, out)
		}
	}
}

// epubFirstDescendantHref returns the href of the first descendant <a>
// that carries one.
func epubFirstDescendantHref(n *html.Node) (string, bool) {
	href := ""
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data == "a" {
				if h := htmlAttribute(c, "href"); h != "" {
					href = h
					return true
				}
			}
			if walk(c) {
				return true
			}
		}
		return false
	}
	if walk(n) {
		return href, true
	}
	return "", false
}

// epubNodeText renders the visible text of a nav element with all
// whitespace collapsed, matching how nav titles display.
func epubNodeText(n *html.Node) string {
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			parts = append(parts, n.Data)
		case html.ElementNode:
			if n.Data == "script" || n.Data == "style" {
				return
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// epubChapterHeadings places a chapter's nav points: points without a
// fragment become document-start headings; the rest are attached to the
// element carrying the matching id (or an <a name> anchor). Points whose
// fragment cannot be matched are reported and dropped.
func epubChapterHeadings(doc *html.Node, points []epubNavPoint, docHref string, warnings *[]string, located, rejected map[int]bool) ([]epubNavHeading, map[*html.Node][]epubNavHeading) {
	ids := epubIDIndex(doc)
	at := make(map[*html.Node][]epubNavHeading)
	var docStart []epubNavHeading
	for _, p := range points {
		heading := epubNavHeading{level: p.level, text: p.title}
		if p.fragment == "" {
			docStart = append(docStart, heading)
			located[p.order] = true
			continue
		}
		node := ids[p.fragment]
		if node == nil {
			*warnings = append(*warnings, fmt.Sprintf("epub navigation link %q skipped: no element with id %q in %s", p.href, p.fragment, docHref))
			rejected[p.order] = true
			continue
		}
		at[node] = append(at[node], heading)
		located[p.order] = true
	}
	return docStart, at
}

// epubIDIndex indexes every element by its id attribute, plus legacy <a
// name> anchors. The first element to claim an identifier wins, matching
// browser getElementById behavior.
func epubIDIndex(doc *html.Node) map[string]*html.Node {
	ids := make(map[string]*html.Node)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if id := htmlAttribute(n, "id"); id != "" {
				if _, ok := ids[id]; !ok {
					ids[id] = n
				}
			}
			if n.Data == "a" {
				if name := htmlAttribute(n, "name"); name != "" {
					if _, ok := ids[name]; !ok {
						ids[name] = n
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return ids
}
