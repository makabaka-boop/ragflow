//
// Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package parser

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/AkmalOt/gomsg"
	"go.uber.org/zap"
	"golang.org/x/net/html"
	"golang.org/x/text/transform"

	"ragflow/internal/common"
	charsetlib "ragflow/internal/parser/charset"
	medialib "ragflow/internal/parser/media"
	"ragflow/internal/utility"
)

// EmailParser parses .eml (RFC 5322 email) and .msg (Outlook OLE2) files
// into structured JSON output. Mirrors Python's _email() method in
// rag/flow/parser/parser.py, whose .msg branch uses extract_msg.
type EmailParser struct {
	fields []string
	// textOnly skips inline (cid-referenced) body images. The text
	// extraction entry points (previews, attachments, CLI, nested email
	// re-chunking) run without a run media cache and must never emit images.
	textOnly bool
}

var defaultEmailFields = []string{"from", "to", "cc", "bcc", "date", "subject", "body", "attachments", "metadata"}

func NewEmailParser() *EmailParser {
	return &EmailParser{fields: append([]string(nil), defaultEmailFields...)}
}

// NewEmailTextParser returns the text-only variant used by readable-text
// extraction (previews, attachments, CLI) and nested email re-chunking.
func NewEmailTextParser() *EmailParser {
	return &EmailParser{fields: append([]string(nil), defaultEmailFields...), textOnly: true}
}

func (p *EmailParser) ConfigureFromSetup(setup map[string]any) {
	if p == nil || setup == nil {
		return
	}
	if raw, ok := setup["fields"]; ok {
		switch list := raw.(type) {
		case []string:
			p.fields = list
		case []any:
			p.fields = make([]string, 0, len(list))
			for _, item := range list {
				if s, ok := item.(string); ok {
					p.fields = append(p.fields, s)
				}
			}
		}
	}
	if len(p.fields) == 0 {
		p.fields = append([]string(nil), defaultEmailFields...)
	}
}

func (p *EmailParser) ParseWithResult(ctx context.Context, filename string, data []byte) ParseResult {
	return p.parseEmail(ctx, filename, data, 0)
}

// parseEmail is ParseWithResult with a re-chunk depth so nested email
// attachments are parsed (and their body made retrievable) without unbounded
// recursion into attachments-of-attachments.
func (p *EmailParser) parseEmail(ctx context.Context, filename string, data []byte, depth int) ParseResult {
	ext := strings.ToLower(filepath.Ext(filename))

	var content map[string]any
	var inlineImages []map[string]any
	var warnings []string
	if ext == ".msg" {
		var (
			msg map[string]any
			err error
		)
		// gomsg.Decode parses untrusted OLE2/CFB input; guard against a
		// panic from a malformed .msg so one bad email can't take down the
		// ingestion worker.
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Log so a genuine bug (e.g. a nil deref in parseMSG)
					// is not silently masked as a "decode panicked" error.
					err = fmt.Errorf("email: .msg decode panicked: %v", r)
					common.Error("email: .msg decode panicked, skipping", err, zap.String("file", filename))
				}
			}()
			msg, err = parseMSG(data, p.fields)
		}()
		if err != nil {
			return ParseResult{Err: fmt.Errorf("email: .msg: %w", err)}
		}
		content = msg
	} else {
		var err error
		content, inlineImages, warnings, err = parseEMLWithError(ctx, bytes.NewReader(data), p.fields, p.textOnly)
		if err != nil {
			return ParseResult{Err: fmt.Errorf("email: .eml: %w", err)}
		}
	}

	// Re-chunk attachments so their content becomes retrievable within the
	// same document (user-oriented; mirrors Python legacy rag/app/email.py
	// naive_chunk). Each attachment is re-parsed by its file extension via
	// the shared parser registry; a single unparseable/skipped attachment
	// never breaks the whole email.
	extraItems := p.rechunkEmailAttachments(ctx, content, depth)

	// Attachments have been consumed by rechunkEmailAttachments, which
	// re-parses each one by extension into a searchable item. Keeping their
	// raw payloads in the parent item would only bloat the parser result.
	delete(content, "attachments")

	content["doc_type_kwd"] = "text"
	items := []map[string]any{content}
	if headerText := searchableEmailHeaders(content); headerText != "" {
		items = append(items, NewTextJSONItem(headerText).ToMap())
	}
	// Body images belong to the message body, so they follow the main item
	// (and its header item) ahead of re-chunked attachments.
	items = append(items, inlineImages...)
	items = append(items, extraItems...)
	return ParseResult{
		File:     map[string]any{"name": filename},
		JSON:     MapsToItems(items),
		Warnings: warnings,
	}
}

// searchableEmailHeaders returns the user-facing headers as a separate JSON
// text item. The main email item keeps its structured fields and body; the
// chunker indexes only item text, so headers need their own text item.
func searchableEmailHeaders(content map[string]any) string {
	var sb strings.Builder
	for _, key := range []string{"from", "to", "cc", "bcc", "date", "subject"} {
		value, ok := content[key].(string)
		if !ok || value == "" {
			continue
		}
		sb.WriteString(key)
		sb.WriteString(":")
		sb.WriteString(value)
		sb.WriteString("\n")
	}
	return sb.String()
}

// -- field set helpers --

func targetFieldsSet(fields []string) map[string]bool {
	m := make(map[string]bool, len(fields))
	for _, f := range fields {
		m[strings.ToLower(strings.TrimSpace(f))] = true
	}
	return m
}

// -- header decoding (RFC 2047) --

// rfc2047Decoder decodes RFC 2047 encoded-words (e.g. "=?utf-8?B?...?=") in
// header values. utf-8 is handled natively by mime; the CharsetReader covers
// the non-UTF-8 charsets via charsetEncoding.
var rfc2047Decoder = &mime.WordDecoder{
	CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		if enc, ok := charsetlib.CharsetEncoding(charset); ok {
			return transform.NewReader(input, enc.NewDecoder()), nil
		}
		return nil, fmt.Errorf("email: unsupported header charset %q", charset)
	},
}

// decodeHeaderWord decodes any RFC 2047 encoded-words in a header value,
// leaving undecodable values untouched.
func decodeHeaderWord(val string) string {
	decoded, err := rfc2047Decoder.DecodeHeader(val)
	if err != nil {
		return val
	}
	return decoded
}

// -- .eml parsing (RFC 5322 with multipart support) --

func parseEMLWithError(ctx context.Context, r io.Reader, fields []string, textOnly bool) (map[string]any, []map[string]any, []string, error) {
	target := targetFieldsSet(fields)
	content := map[string]any{}

	msg, err := mail.ReadMessage(r)
	if err != nil {
		return nil, nil, nil, err
	}

	// Headers. net/mail does not decode RFC 2047 encoded-words, so decode
	// each value explicitly; otherwise a non-ASCII subject/from arrives as
	// an unreadable "=?utf-8?B?...?=" blob that chunk delimiters then shred
	// at the "?" characters.
	meta := map[string]any{}
	for key, vals := range msg.Header {
		keyLower := strings.ToLower(key)
		decoded := make([]string, len(vals))
		for i, v := range vals {
			decoded[i] = decodeHeaderWord(v)
		}
		val := strings.Join(decoded, ", ")
		switch keyLower {
		case "from", "to", "cc", "bcc", "date", "subject":
			if target[keyLower] {
				content[keyLower] = val
			}
		default:
			meta[keyLower] = val
		}
	}
	// Always emit metadata to match the Python flow parser contract
	// (rag/flow/parser/parser.py:_email), which unconditionally builds a
	// metadata dict and collects every non-basic header into it regardless
	// of whether "metadata" is present in the configured fields.
	content["metadata"] = meta

	// Body and attachments — the emailBodyReader walks all multipart parts,
	// collects text/html bodies alongside attachment parts whose
	// Content-Disposition starts with "attachment", and, when "body" is
	// requested in a non-text-only parse, extracts the inline images the
	// HTML body references through cid: URLs.
	needAttachments := target["attachments"]
	// Attachments are extracted independently of "body" (mirrors the Python
	// flow parser _email, whose attachment block is separate from the body
	// block). Walk the body whenever either is requested, then gate each key
	// by its own field so text/text_html stay absent when "body" is not in
	// fields while attachments are still extracted when "attachments" is.
	var inlineImages []map[string]any
	var warnings []string
	if target["body"] || needAttachments {
		reader := &emailBodyReader{
			cfg: emailBodyConfig{
				ctx:                ctx,
				collectAttachments: needAttachments,
				extractImages:      target["body"] && !textOnly,
			},
		}
		contentType := msg.Header.Get("Content-Type")
		cte := msg.Header.Get("Content-Transfer-Encoding")
		reader.walk(msg.Body, contentType, cte, nil)
		result := reader.result()
		if result.err != nil {
			return nil, nil, nil, result.err
		}
		// Always emit text/text_html when "body" is requested, to match the
		// Python flow parser contract (rag/flow/parser/parser.py:_email),
		// which sets both unconditionally (empty string for a missing part)
		// rather than omitting the key.
		if target["body"] {
			content["text"] = result.text
			content["text_html"] = result.html
		}
		if needAttachments {
			content["attachments"] = result.attachments
		}
		inlineImages = result.imageItems
		warnings = result.warnings
	}

	return content, inlineImages, warnings, nil
}

// emailBodyConfig controls one email body walk.
type emailBodyConfig struct {
	ctx context.Context
	// collectAttachments gathers parts whose Content-Disposition is
	// "attachment" independently of inline body images.
	collectAttachments bool
	// extractImages opens a Content-ID scope for each multipart/related
	// container and extracts the image/* parts the group's HTML body
	// references through cid: URLs.
	extractImages bool
}

// emailInlineImage is one image/* MIME part addressable from a
// multipart/related HTML body through its Content-ID. raw holds the
// still-CTE-encoded transport bytes; the CTE is validated lazily only for
// the images the body actually references.
type emailInlineImage struct {
	cid  string
	mime string
	raw  []byte
	cte  string
}

// emailRelatedGroup scopes the Content-ID parts of one multipart/related
// container. Content-IDs are only meaningful within their group, so two
// groups may reuse the same id without one group's HTML resolving to the
// other group's picture.
type emailRelatedGroup struct {
	id     int
	images map[string]*emailInlineImage
	// html collects the group's HTML parts (possibly nested inside an
	// alternative container) in walk order.
	html []string
}

// emailBodyReader walks an email MIME tree collecting the plain-text and
// HTML bodies, regular attachments, and — per multipart/related group — the
// inline images referenced by the group's HTML body.
type emailBodyReader struct {
	cfg emailBodyConfig

	textParts   []string
	htmlParts   []string
	attachments []map[string]any
	imageItems  []map[string]any

	budget   *medialib.Budget
	sink     *medialib.Sink
	groupSeq int
	order    int

	// missing counts cid: references with no matching Content-ID part.
	missing int
	// corrupt counts referenced parts whose CTE payload fails to decode or
	// decodes to zero bytes.
	corrupt int
	err     error
}

type emailBodyResult struct {
	text        string
	html        string
	attachments []map[string]any
	imageItems  []map[string]any
	warnings    []string
	err         error
}

// walk traverses one (possibly multipart) MIME entity. group is the
// enclosing multipart/related Content-ID scope, or nil outside one.
func (r *emailBodyReader) walk(body io.Reader, contentType, cte string, group *emailRelatedGroup) {
	if r.err != nil {
		return
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		r.readLeaf(body, mediaType, params["charset"], cte)
		return
	}

	boundary := params["boundary"]
	if boundary == "" {
		raw, _ := io.ReadAll(body)
		if decoded := decodeMailPayload(decodeCTE(raw, cte), ""); decoded != "" {
			r.textParts = append(r.textParts, decoded)
		}
		return
	}

	// A multipart/related container opens a fresh Content-ID scope that
	// closes once every part (including a nested alternative container) has
	// been visited. Nested related containers get their own scope.
	localGroup := group
	if mediaType == "multipart/related" && r.cfg.extractImages {
		r.groupSeq++
		localGroup = &emailRelatedGroup{id: r.groupSeq, images: make(map[string]*emailInlineImage)}
	}

	mr := multipart.NewReader(body, boundary)
	for r.err == nil {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		partCT := part.Header.Get("Content-Type")
		partMedia, partParams, _ := mime.ParseMediaType(partCT)

		if strings.HasPrefix(partMedia, "multipart/") {
			// RFC 2045 6.4 forbids base64 and quoted-printable on a multipart
			// entity, so a nested container is never CTE-decoded.
			r.walk(part, partCT, "", localGroup)
			continue
		}

		// A part explicitly marked as an attachment stays a regular
		// attachment even when it carries a Content-ID inside a related
		// group.
		if r.cfg.collectAttachments && isAttachmentPart(part) {
			raw, _ := io.ReadAll(part)
			raw = decodeCTE(raw, part.Header.Get("Content-Transfer-Encoding"))

			r.attachments = append(r.attachments, map[string]any{
				"filename": attachmentFilename(part),
				"payload":  decodeMailPayload(raw, partParams["charset"]),
				// raw preserves the byte-exact decoded-CTE bytes so the
				// re-chunk step re-parses the original attachment instead of
				// the charset-decoded string (which can differ when the
				// attachment declares a CJK charset). rechunkEmailAttachments
				// prefers "raw" and falls back to "payload".
				"raw": string(raw),
			})
			continue
		}

		// An inline image part inside a related group is a cid candidate.
		// Only image/* parts with a Content-ID are registered; unreferenced
		// parts are never emitted, and a duplicate id keeps the first part
		// (multipart/related references resolve position-independently).
		if localGroup != nil && strings.HasPrefix(partMedia, "image/") && r.cfg.extractImages {
			if cid := emailContentID(part.Header.Get("Content-ID")); cid != "" {
				if _, exists := localGroup.images[cid]; !exists {
					raw, _ := io.ReadAll(part)
					localGroup.images[cid] = &emailInlineImage{
						cid:  cid,
						mime: partMedia,
						raw:  append([]byte(nil), raw...),
						cte:  part.Header.Get("Content-Transfer-Encoding"),
					}
				} else {
					_, _ = io.Copy(io.Discard, part)
				}
				continue
			}
		}

		raw, _ := io.ReadAll(part)
		raw = decodeCTE(raw, part.Header.Get("Content-Transfer-Encoding"))
		decoded := decodeMailPayload(raw, partParams["charset"])

		switch partMedia {
		case "text/plain":
			if decoded != "" {
				r.textParts = append(r.textParts, decoded)
			}
		case "text/html":
			if decoded != "" {
				r.htmlParts = append(r.htmlParts, decoded)
				if localGroup != nil {
					localGroup.html = append(localGroup.html, decoded)
				}
			}
		}
	}

	if localGroup != group {
		r.resolveGroup(localGroup)
	}
}

// readLeaf handles a non-multipart top-level entity. Every non-HTML body
// (including text/plain) lands in the text body, matching the historical
// single-part behavior.
func (r *emailBodyReader) readLeaf(body io.Reader, mediaType, charset, cte string) {
	raw, _ := io.ReadAll(body)
	raw = decodeCTE(raw, cte)
	decoded := decodeMailPayload(raw, charset)
	if decoded == "" {
		return
	}
	if mediaType == "text/html" {
		r.htmlParts = append(r.htmlParts, decoded)
		return
	}
	r.textParts = append(r.textParts, decoded)
}

// resolveGroup scans the group's HTML bodies for cid: image references and
// emits each referenced image once, in document order.
func (r *emailBodyReader) resolveGroup(group *emailRelatedGroup) {
	if r.err != nil {
		return
	}
	seen := make(map[string]struct{})
	for _, htmlText := range group.html {
		for _, ref := range cidImageRefs(htmlText) {
			if r.err != nil {
				return
			}
			if _, dup := seen[ref.cid]; dup {
				continue
			}
			seen[ref.cid] = struct{}{}
			part, ok := group.images[ref.cid]
			if !ok {
				// The HTML references a Content-ID this group does not
				// carry; keep parsing the rest of the message.
				r.missing++
				continue
			}
			r.emitImage(group, part, ref.alt)
		}
	}
}

// emitImage validates one referenced part, admits it through the shared run
// media budget, and writes it to the run media cache as a ref-only image
// item. Missing references, unreadable/empty payloads, and media-limit
// rejections skip the image with a warning instead of failing the message.
func (r *emailBodyReader) emitImage(group *emailRelatedGroup, part *emailInlineImage, alt string) {
	data, ok := decodeCTEStrict(part.raw, part.cte)
	if !ok || len(data) == 0 {
		r.corrupt++
		return
	}
	sink := r.mediaSink()
	if sink == nil {
		return
	}
	// The group id in the key keeps same-named Content-IDs in different
	// related groups from sharing (or cross-resolving) a cached entry.
	source := fmt.Sprintf("cid:%d:%s", group.id, part.cid)
	ref, err := sink.Resolve(source, medialib.Size(len(data)), func() (string, []byte, bool) {
		return part.mime, data, true
	})
	if err != nil {
		r.err = err
		return
	}
	if ref == nil {
		// Over the per-image/run limits; the budget and sink own the warning.
		return
	}
	r.order++
	r.imageItems = append(r.imageItems, ImageItem{
		Text:  strings.TrimSpace(alt),
		Media: ref,
		Extra: map[string]any{"media_order": r.order},
	}.ToMap())
}

// mediaSink lazily builds the shared run media sink. A full (non-text-only)
// parse that reaches an inline image without an injected run media cache
// fails, matching the HTML/EPUB media contract: callers must not silently
// drop referenced images.
func (r *emailBodyReader) mediaSink() *medialib.Sink {
	if r.sink != nil {
		return r.sink
	}
	if medialib.CacheFromContext(r.cfg.ctx) == nil {
		r.err = fmt.Errorf("email media cache is not configured")
		return nil
	}
	r.budget = medialib.NewBudget()
	r.sink = medialib.NewSink(r.cfg.ctx, "email", r.budget)
	return r.sink
}

func (r *emailBodyReader) result() emailBodyResult {
	var warnings []string
	if r.sink != nil {
		warnings = append(warnings, r.sink.Warnings()...)
	}
	if r.missing > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"email parser skipped %d inline image reference(s) without a matching Content-ID part", r.missing))
	}
	if r.corrupt > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"email parser skipped %d inline image(s) with a corrupt or empty payload", r.corrupt))
	}
	return emailBodyResult{
		text:        strings.Join(r.textParts, "\n"),
		html:        strings.Join(r.htmlParts, "\n"),
		attachments: r.attachments,
		imageItems:  r.imageItems,
		warnings:    warnings,
		err:         r.err,
	}
}

// emailCIDRef is one <img src="cid:..."> reference found in an HTML body.
type emailCIDRef struct {
	cid string
	alt string
}

// cidImageRefs scans an HTML body for <img src="cid:..."> references in
// document order. Only the cid: scheme is recognized; external http(s),
// protocol-relative and relative sources are ignored and never fetched. The
// tokenizer tolerates the malformed markup real-world mailers emit.
func cidImageRefs(htmlText string) []emailCIDRef {
	var refs []emailCIDRef
	tokenizer := html.NewTokenizer(strings.NewReader(htmlText))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return refs
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			if !strings.EqualFold(string(name), "img") {
				continue
			}
			var src, alt string
			for {
				key, val, more := tokenizer.TagAttr()
				switch strings.ToLower(string(key)) {
				case "src":
					src = string(val)
				case "alt":
					alt = string(val)
				}
				if !more {
					break
				}
			}
			if cid, ok := parseCIDURL(src); ok {
				refs = append(refs, emailCIDRef{cid: cid, alt: alt})
			}
		}
	}
}

// parseCIDURL extracts the bare Content-ID from a cid: URL, as written in
// an HTML <img src>: the "cid:" prefix, optional angle brackets and percent
// escapes (RFC 2392) are removed. Matching is case-sensitive and scoped to
// the enclosing related group, like MIME Content-IDs themselves.
func parseCIDURL(src string) (string, bool) {
	src = strings.TrimSpace(src)
	if len(src) < 4 || !strings.EqualFold(src[:4], "cid:") {
		return "", false
	}
	ref := strings.TrimSpace(src[4:])
	ref = strings.TrimPrefix(ref, "<")
	ref = strings.TrimSuffix(ref, ">")
	if unescaped, err := url.PathUnescape(ref); err == nil {
		ref = unescaped
	}
	ref = strings.TrimSpace(ref)
	return ref, ref != ""
}

// emailContentID normalizes a MIME Content-ID header value (typically
// "<id@host>") into the bare identifier used by cid: references.
func emailContentID(header string) string {
	id := strings.TrimSpace(header)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.TrimSpace(id)
}

// decodeCTEStrict decodes a MIME Content-Transfer-Encoding, reporting a
// failed base64/QP decode instead of returning the raw payload. Unknown
// encodings and the identity encodings pass the bytes through unchanged.
func decodeCTEStrict(raw []byte, cte string) ([]byte, bool) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "", "7bit", "8bit", "binary":
		return raw, true
	case "base64":
		// Real-world MIME base64 is often line-wrapped (~76 chars per line).
		// Remove all line breaks before decoding; base64.StdEncoding strictly
		// rejects interior whitespace.
		cleanRaw := bytes.ReplaceAll(raw, []byte("\n"), nil)
		cleanRaw = bytes.ReplaceAll(cleanRaw, []byte("\r"), nil)
		d, err := base64.StdEncoding.DecodeString(string(cleanRaw))
		if err != nil {
			return nil, false
		}
		return d, true
	case "quoted-printable":
		d, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		if err != nil {
			return nil, false
		}
		return d, true
	default:
		return raw, true
	}
}

// decodeCTE decodes Content-Transfer-Encoding leniently for text bodies and
// attachments, mirroring Python part.get_payload(decode=True): a payload that
// fails to decode is returned untouched.
func decodeCTE(raw []byte, cte string) []byte {
	d, ok := decodeCTEStrict(raw, cte)
	if !ok {
		return raw
	}
	return d
}

func isAttachmentPart(part *multipart.Part) bool {
	disp := part.Header.Get("Content-Disposition")
	if disp == "" {
		return false
	}
	dispType, _, err := mime.ParseMediaType(disp)
	if err != nil {
		return false
	}
	return strings.EqualFold(dispType, "attachment")
}

// attachmentFilename extracts a filename from the part's
// Content-Disposition or Content-Type headers. Mail clients write a
// non-ASCII name as RFC 2047 encoded-words inside the parameter, as in
// filename="=?utf-8?B?...?=", and the mime package leaves them encoded. Such
// a name has no file extension, so rechunkEmailAttachments skipped the
// attachment; it is decoded here. The filename is decoded before
// filepath.Base drops any directory from it, as part.FileName does, because a
// base64 encoded-word can contain a "/".
func attachmentFilename(part *multipart.Part) string {
	if _, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition")); err == nil {
		if fn := params["filename"]; fn != "" {
			return filepath.Base(decodeHeaderWord(fn))
		}
	}
	ct := part.Header.Get("Content-Type")
	if ct != "" {
		_, params, _ := mime.ParseMediaType(ct)
		if name, ok := params["name"]; ok {
			return decodeHeaderWord(name)
		}
	}
	return "attachment.bin"
}

// decodeMailPayload attempts charset decoding using the unified DecodeToUTF8 helper.
func decodeMailPayload(payload []byte, charset string) string {
	if len(payload) == 0 {
		return ""
	}
	decoded, _ := charsetlib.DecodeToUTF8(payload, charset)
	return string(decoded)
}

// parseMSG parses an Outlook .msg (OLE2 compound document) file using the
// gomsg library, mirroring the Python flow parser's _email() .msg branch
// (rag/flow/parser/parser.py), which parses via extract_msg. The output map
// shares the same field shape as parseEML so the downstream json/text
// assembly in ParseWithResult is reused unchanged.
func parseMSG(data []byte, fields []string) (map[string]any, error) {
	target := targetFieldsSet(fields)

	msg, err := gomsg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	content := map[string]any{}

	if target["from"] {
		content["from"] = formatSender(msg)
	}
	if target["to"] {
		content["to"] = formatRecipient(msg.DisplayTo)
	}
	if target["cc"] {
		content["cc"] = formatRecipient(msg.DisplayCC)
	}
	if target["bcc"] {
		content["bcc"] = formatRecipient(msg.DisplayBCC)
	}
	if target["date"] {
		content["date"] = formatMsgDate(msg.Date)
	}
	if target["subject"] {
		content["subject"] = msg.Subject
	}

	// Always emit metadata to match the Python flow parser contract, which
	// unconditionally builds a {message_id, in_reply_to} metadata dict for
	// .msg files regardless of whether "metadata" is in the configured fields.
	// Empty values are emitted as nil (JSON null) to match extract_msg's None.
	content["metadata"] = map[string]any{
		"message_id":  orNil(msg.MessageID),
		"in_reply_to": orNil(msg.InReplyTo),
	}

	if target["body"] {
		// Mirror Python: prefer the plain body, fall back to the HTML body
		// when the plain body is empty. The .msg branch emits only "text"
		// (never "text_html"), matching the Python _email .msg contract exactly.
		text := msg.Body
		if strings.TrimSpace(text) == "" && len(msg.BodyHTML) > 0 {
			text = string(msg.BodyHTML)
		}
		content["text"] = text
	}

	if target["attachments"] {
		// Flatten attachments, recursing into embedded .msg files. gomsg
		// parses an embedded Message via Attachment.EmbeddedMessage even
		// though it exposes no raw bytes for it; the embedded message body
		// is surfaced as a retrievable text attachment (see msgAttachments).
		content["attachments"] = msgAttachments(msg)
	}

	return content, nil
}

// msgAttachments flattens a gomsg.Message's attachments into the
// {filename, payload} shape rechunkEmailAttachments consumes. Embedded .msg
// attachments expose no raw bytes via gomsg, but gomsg does parse the embedded
// Message; we surface its body as a retrievable text attachment (named .txt so
// the re-chunk step re-parses it as plain text) and recurse into its own
// attachments, so an embedded email is no longer silently dropped.
func msgAttachments(msg *gomsg.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		if a.IsEmbeddedMessage() {
			if em := a.EmbeddedMessage(); em != nil {
				body := em.Body
				if strings.TrimSpace(body) == "" {
					body = string(em.BodyHTML)
				}
				if body != "" {
					out = append(out, map[string]any{
						"filename": a.DisplayName() + ".txt",
						"payload":  body,
					})
				}
				out = append(out, msgAttachments(em)...)
				continue
			}
		}
		out = append(out, map[string]any{
			"filename": a.DisplayName(),
			"payload":  string(a.Data()),
		})
	}
	return out
}

// primarySenderEmail prefers the SMTP address, falling back to the raw email
// address when SMTP is unavailable (e.g. an EX address type).
func primarySenderEmail(msg *gomsg.Message) string {
	if msg.SenderSMTP != "" {
		return msg.SenderSMTP
	}
	return msg.SenderEmail
}

// formatSender renders the .msg sender the way extract_msg's "sender" string
// is displayed: "Display Name <email>" when a distinct display name is present,
// otherwise "<email>" (matching extract_msg's angular-bracket form for an
// address with no display name).
func formatSender(msg *gomsg.Message) string {
	email := primarySenderEmail(msg)
	if msg.SenderName != "" && msg.SenderName != email {
		return msg.SenderName + " <" + email + ">"
	}
	if email != "" {
		return "<" + email + ">"
	}
	return msg.SenderName
}

// formatRecipient renders a display recipient string the way extract_msg does:
// a bare single email address is wrapped in angle brackets, while a string
// that already contains an address form (display name, or multiple recipients)
// is returned unchanged.
func formatRecipient(display string) string {
	if display == "" {
		return ""
	}
	if strings.Contains(display, "<") {
		return display
	}
	if !strings.ContainsAny(display, ",;") && strings.Contains(display, "@") {
		return "<" + display + ">"
	}
	return display
}

// orNil maps an empty string to nil so it serializes as JSON null, matching
// extract_msg's None for absent properties.
func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// formatMsgDate renders an Outlook .msg date the way extract_msg does:
// strftime("%Y-%m-%d %H:%M:%S%z"), which emits the zone without a colon
// (e.g. "2018-03-24 00:06:29+0800"). Go's -0700 layout reproduces that
// (the -07:00 layout would wrongly insert a colon). A zero time (date
// missing from the .msg) maps to nil so it serializes as JSON null,
// matching extract_msg's None, instead of a bogus sentinel such as
// "0001-01-01 00:00:00+0000".
func formatMsgDate(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format("2006-01-02 15:04:05-0700")
}

// recoverParse runs fn, converting a panic from an untrusted attachment
// parser (e.g. a corrupt PDF/DOCX hitting a native CGO backend) into a zero
// ParseResult with panicked=true, so the caller can skip that one attachment
// instead of failing the whole email. This mirrors the recover around the
// .msg parseMSG call in parseEmail.
func recoverParse(fn func() ParseResult) (res ParseResult, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	return fn(), false
}

// rechunkEmailAttachments re-parses each email attachment by its file
// extension and folds the extracted text back into the same document so
// attachment content becomes retrievable. This mirrors the user-oriented
// behaviour of Python's legacy rag/app/email.py, which re-chunks every
// attachment via naive_chunk and merges the resulting chunks into the same
// document.
//
// KNOWN LIMITATION: re-chunk uses the text-only, default-config parser
// returned by GetTextOnlyParser(ft) (and, for a nested email, only the
// top-level p.fields — no tenant/setup language, parse_method, or OCR/VLM
// model). The extracted attachment text may therefore differ from ingesting
// the same file as a standalone document through the pipeline's
// tenant/setup-configured parser. This is a deliberate, best-effort
// approximation for making attachment content retrievable (the same
// simplification Python's email.py naive_chunk makes), not a correctness
// bug; threading the full tenant/setup config into re-chunk is intentionally
// out of scope here.
//
// Attachments whose extension has no text-oriented parser (images, audio,
// video) are skipped: they carry no plain text in this pipeline and would
// require vision/speech models that are out of scope here. A single
// unparseable, empty, unsupported, or panicking attachment is skipped
// without failing the whole email (mirrors email.py's per-attachment
// try/except).
//
// The function returns structured JSON items (one per non-empty text
// segment), each carrying only "text" and "doc_type_kwd".
//
// Re-chunking stops at one level of nesting: a nested email is parsed (so its
// body becomes retrievable) but its own attachments are not re-chunked, to
// avoid unbounded recursion through attachments-of-attachments.
const maxRechunkPayloadBytes = 32 << 20 // 32 MiB

func (p *EmailParser) rechunkEmailAttachments(ctx context.Context, content map[string]any, depth int) []map[string]any {
	if depth > 0 {
		return nil
	}
	// Honor task cancellation so a long attachment list does not keep
	// re-parsing after the ingestion task has been stopped/aborted.
	if ctx.Err() != nil {
		return nil
	}

	raw, ok := content["attachments"].([]map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}

	var extra []map[string]any
	for _, att := range raw {
		fn, _ := att["filename"].(string)
		payload, _ := att["payload"].(string)
		// Prefer the byte-exact raw bytes (when present) so a re-parsed
		// attachment is not silently corrupted by a prior charset decode.
		if rawPayload, ok := att["raw"].(string); ok && rawPayload != "" {
			payload = rawPayload
		}
		if fn == "" || payload == "" {
			// Empty payload (e.g. an embedded .msg with no exposed bytes) or
			// a missing filename — nothing to re-parse.
			continue
		}
		if len(payload) > maxRechunkPayloadBytes {
			// Too large to re-parse inline; re-chunking re-runs the heavy
			// parsers (PDF/OCR/...), which would otherwise dominate or stall
			// ingestion on a single huge attachment. The attachment remains
			// referenced in metadata.
			continue
		}
		ft := utility.GetFileType(fn)
		switch ft {
		case utility.FileTypeOTHER, utility.FileTypeVISUAL,
			utility.FileTypeAURAL, utility.FileTypeVIDEO, utility.FileTypeFOLDER:
			// No plain-text parser in this pipeline; skip rather than call
			// a vision/speech model that is out of scope.
			continue
		}
		var res ParseResult
		var panicked bool
		if ft == utility.FileTypeEMAIL {
			// Reuse the top-level field configuration (including "body") so a
			// nested .eml/.msg is parsed for its body instead of with a fresh,
			// unconfigured parser that would only emit metadata and index
			// garbage. Nested emails stay text-only: re-chunking has no run
			// media-cache semantics and their cid images are not part of the
			// top-level body extraction.
			ep := NewEmailTextParser()
			ep.ConfigureFromSetup(map[string]any{"fields": p.fields})
			res, panicked = recoverParse(func() ParseResult {
				return ep.parseEmail(ctx, fn, []byte(payload), depth+1)
			})
		} else {
			np, err := GetTextOnlyParser(ft)
			if err != nil {
				continue
			}
			res, panicked = recoverParse(func() ParseResult {
				return np.ParseWithResult(ctx, fn, []byte(payload))
			})
		}
		if panicked {
			// An untrusted attachment parser (e.g. a corrupt PDF/DOCX hitting
			// a native CGO backend) panicked. Skip just this attachment so one
			// bad file can't fail the whole email — mirrors the .msg
			// parseMSG recover.
			common.Warn("email: attachment re-parse panicked, skipping", zap.String("file", fn))
		}
		if panicked || res.Err != nil {
			continue
		}

		var texts []string
		if len(res.JSON) > 0 {
			for _, it := range res.JSON {
				if t, ok := it.ToMap()["text"].(string); ok {
					if t = strings.TrimSpace(t); t != "" {
						texts = append(texts, t)
					}
				}
			}
		}

		for _, t := range texts {
			extra = append(extra, map[string]any{
				"text":         t,
				"doc_type_kwd": "text",
			})
		}
	}
	return extra
}
