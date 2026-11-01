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
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"path"
	"strings"
)

// pptxSpeakerNotesMarker prefixes the speaker-notes paragraph emitted after
// a slide's content so the note stays identifiable inside merged chunks.
const pptxSpeakerNotesMarker = "Speaker notes:"

// Relationship type URIs defined by the OOXML package convention.
const (
	pptxRelSlide      = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide"
	pptxRelNotesSlide = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide"
)

// pptxSlideNotes is one slide's speaker-note text, keyed by the slide's
// 1-based position in presentation order (p:sldIdLst), never by the
// notesSlide file name.
type pptxSlideNotes struct {
	slideNumber int
	text        string
}

// extractPPTXSpeakerNotes reads speaker notes straight from the OOXML
// package, independently of the office_oxide IR (which does not carry
// notes). Slides are resolved in presentation order via
// presentation.xml + its relationships, and each slide's notesSlide
// relationship points at the actual notes part. Notes-to-slide mapping
// therefore never relies on slideN.xml / notesSlideN.xml file numbering.
//
// Extraction is best effort and granular: an unreadable package, broken
// presentation order, or one corrupt notes relationship/part yields a
// warning while every other slide and note is still returned. A slide
// without a notes relationship, or with an empty body placeholder,
// produces nothing and no warning.
func extractPPTXSpeakerNotes(data []byte) ([]pptxSlideNotes, []string) {
	var warnings []string
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, []string{fmt.Sprintf("speaker notes unavailable: not a valid OOXML package: %v", err)}
	}
	parts := pptxZipParts(zr.File)

	presXML, found, err := parts.read("ppt/presentation.xml")
	if err != nil {
		return nil, []string{fmt.Sprintf("speaker notes unavailable: ppt/presentation.xml: %v", err)}
	}
	if !found {
		return nil, []string{"speaker notes unavailable: ppt/presentation.xml not found in package"}
	}
	var presentation pptxPresentationXML
	if err := xml.Unmarshal(presXML, &presentation); err != nil {
		return nil, []string{fmt.Sprintf("speaker notes unavailable: ppt/presentation.xml: %v", err)}
	}

	presRels, err := pptxLoadRelationships(parts, "ppt/_rels/presentation.xml.rels")
	if err != nil {
		return nil, []string{fmt.Sprintf("speaker notes unavailable: presentation relationships: %v", err)}
	}
	slideTargets := map[string]string{}
	for _, rel := range presRels.Relationships {
		if rel.Type == pptxRelSlide && rel.Target != "" {
			slideTargets[rel.ID] = pptxResolvePart("ppt", rel.Target)
		}
	}

	var notes []pptxSlideNotes
	for i, slideID := range presentation.SlideIDs {
		slideNumber := i + 1
		target := slideTargets[slideID.RID]
		if target == "" {
			warnings = append(warnings, fmt.Sprintf(
				"speaker notes for slide %d skipped: presentation.xml.rels has no slide target for relationship %q",
				slideNumber, slideID.RID))
			continue
		}
		text, ok, warn := pptxReadSlideNotes(parts, target, slideNumber)
		if warn != "" {
			warnings = append(warnings, warn)
		}
		if ok {
			notes = append(notes, pptxSlideNotes{slideNumber: slideNumber, text: text})
		}
	}
	return notes, warnings
}

// pptxReadSlideNotes resolves one slide's notesSlide relationship and
// extracts the body-placeholder text. ok is false (with no note) when the
// slide legitimately has no notes; warn is non-empty for corrupt
// relationships or parts.
func pptxReadSlideNotes(parts pptxZipPartReader, slidePart string, slideNumber int) (text string, ok bool, warn string) {
	relsPath := pptxRelationshipsPath(slidePart)
	relsData, found, err := parts.read(relsPath)
	if err != nil {
		return "", false, fmt.Sprintf(
			"speaker notes for slide %d skipped: %s: %v", slideNumber, relsPath, err)
	}
	if !found {
		// A slide with no relationships part has no notes: normal, silent.
		return "", false, ""
	}
	var rels pptxRelationshipsXML
	if err := xml.Unmarshal(relsData, &rels); err != nil {
		return "", false, fmt.Sprintf(
			"speaker notes for slide %d skipped: %s: %v", slideNumber, relsPath, err)
	}
	notesTarget := ""
	for _, rel := range rels.Relationships {
		if rel.Type == pptxRelNotesSlide && rel.Target != "" {
			notesTarget = pptxResolvePart(path.Dir(slidePart), rel.Target)
			break
		}
	}
	if notesTarget == "" {
		return "", false, ""
	}
	notesXML, found, err := parts.read(notesTarget)
	if err != nil {
		return "", false, fmt.Sprintf(
			"speaker notes for slide %d skipped: %s: %v", slideNumber, notesTarget, err)
	}
	if !found {
		return "", false, fmt.Sprintf(
			"speaker notes for slide %d skipped: %s not found in package", slideNumber, notesTarget)
	}
	text, err = pptxNotesBodyText(notesXML)
	if err != nil {
		return "", false, fmt.Sprintf(
			"speaker notes for slide %d skipped: %s: %v", slideNumber, notesTarget, err)
	}
	if strings.TrimSpace(text) == "" {
		return "", false, ""
	}
	return text, true, ""
}

// pptxNotesBodyText extracts the speaker-note paragraphs from a notesSlide
// part. Only the body placeholder shapes contribute text: the slide image
// placeholder, slide number, date and header/footer placeholders (plus the
// embedded thumbnail picture) live in other shapes and are excluded by
// construction. Paragraphs, including blank lines and hard line breaks, are
// joined with "\n".
func pptxNotesBodyText(data []byte) (string, error) {
	var slide pptxNotesSlideXML
	if err := xml.Unmarshal(data, &slide); err != nil {
		return "", err
	}
	var paragraphs []string
	for _, shape := range slide.rootShapes() {
		if shape.Ph == nil || shape.Ph.Type != "body" || shape.TxBody == nil {
			continue
		}
		for _, p := range shape.TxBody.Paragraphs {
			paragraphs = append(paragraphs, p.text())
		}
	}
	return strings.TrimSpace(strings.Join(paragraphs, "\n")), nil
}

// appendPPTXSpeakerNotes inserts each slide's note text item directly after
// that slide's last existing item, so body text, tables and images keep
// their current order and the note follows all of them. Every note item
// carries the same slide_number as the slide; slides with no IR items still
// receive their note, appended in slide order at the end. Items without
// slide_number (the plain-text salvage path) are returned unchanged.
func appendPPTXSpeakerNotes(items []map[string]any, notes []pptxSlideNotes) []map[string]any {
	if len(notes) == 0 {
		return items
	}
	lastIndex := map[int]int{}
	for i, item := range items {
		n, ok := item["slide_number"].(int)
		if !ok {
			return items
		}
		lastIndex[n] = i
	}

	bySlide := map[int]pptxSlideNotes{}
	maxNumber := 0
	for _, note := range notes {
		bySlide[note.slideNumber] = note
		if note.slideNumber > maxNumber {
			maxNumber = note.slideNumber
		}
	}

	out := make([]map[string]any, 0, len(items)+len(notes))
	emitted := map[int]bool{}
	for i, item := range items {
		out = append(out, item)
		if n, ok := item["slide_number"].(int); ok && lastIndex[n] == i {
			if note, ok := bySlide[n]; ok {
				out = append(out, pptxSpeakerNotesItem(note))
				emitted[n] = true
			}
		}
	}
	// Notes whose slide has no IR item at all (section count mismatch).
	for n := 1; n <= maxNumber; n++ {
		if note, ok := bySlide[n]; ok && !emitted[n] {
			out = append(out, pptxSpeakerNotesItem(note))
		}
	}
	return out
}

func pptxSpeakerNotesItem(note pptxSlideNotes) map[string]any {
	return map[string]any{
		"text":          pptxSpeakerNotesMarker + "\n" + note.text,
		DocTypeKey:      DocTypeText,
		"slide_number":  note.slideNumber,
		"speaker_notes": true,
	}
}

// --- OOXML package (zip) reading ------------------------------------------

// pptxZipPartReader indexes a package's parts by normalized name.
type pptxZipPartReader map[string]*zip.File

func pptxZipParts(files []*zip.File) pptxZipPartReader {
	parts := make(pptxZipPartReader, len(files))
	for _, f := range files {
		name := strings.TrimPrefix(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		parts[name] = f
	}
	return parts
}

// read returns the part contents. found is false when the part is absent;
// a non-nil error means the part exists but could not be read.
func (p pptxZipPartReader) read(name string) ([]byte, bool, error) {
	f, ok := p[name]
	if !ok {
		return nil, false, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, true, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		return nil, true, err
	}
	return buf.Bytes(), true, nil
}

// pptxResolvePart resolves a relationship Target (absolute or relative to
// the source part's directory) into a normalized package part path.
func pptxResolvePart(baseDir, target string) string {
	if strings.HasPrefix(target, "/") {
		return path.Clean(strings.TrimPrefix(target, "/"))
	}
	return path.Clean(path.Join(baseDir, target))
}

// pptxRelationshipsPath returns the .rels part path for a package part,
// e.g. ppt/slides/slide1.xml -> ppt/slides/_rels/slide1.xml.rels.
func pptxRelationshipsPath(part string) string {
	return path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
}

func pptxLoadRelationships(parts pptxZipPartReader, relsPath string) (pptxRelationshipsXML, error) {
	var rels pptxRelationshipsXML
	data, found, err := parts.read(relsPath)
	if err != nil {
		return rels, err
	}
	if !found {
		return rels, fmt.Errorf("%s not found in package", relsPath)
	}
	if err := xml.Unmarshal(data, &rels); err != nil {
		return rels, err
	}
	return rels, nil
}

// --- OOXML XML shapes -----------------------------------------------------
//
// Prefix-less struct tags match elements by local name, so the p:
// (PresentationML) and a: (DrawingML) namespaces both resolve without
// hard-coding namespace URIs on every path.

type pptxPresentationXML struct {
	SlideIDs []pptxSlideIDXML `xml:"sldIdLst>sldId"`
}

type pptxSlideIDXML struct {
	RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
}

type pptxRelationshipsXML struct {
	Relationships []pptxRelationshipXML `xml:"Relationship"`
}

type pptxRelationshipXML struct {
	ID     string `xml:"Id,attr"`
	Type   string `xml:"Type,attr"`
	Target string `xml:"Target,attr"`
}

type pptxNotesSlideXML struct {
	Container pptxNotesContainerXML `xml:"cSld>spTree"`
}

// rootShapes returns every shape under the shape tree in document order,
// descending into groups and AlternateContent choices.
func (s pptxNotesSlideXML) rootShapes() []pptxNotesShapeXML {
	return s.Container.shapes()
}

type pptxNotesContainerXML struct {
	Shapes     []pptxNotesShapeXML     `xml:"sp"`
	Groups     []pptxNotesGroupXML     `xml:"grpSp"`
	Alternates []pptxNotesContainerXML `xml:"AlternateContent>Choice"`
}

type pptxNotesGroupXML struct {
	Shapes     []pptxNotesShapeXML     `xml:"sp"`
	Groups     []pptxNotesGroupXML     `xml:"grpSp"`
	Alternates []pptxNotesContainerXML `xml:"AlternateContent>Choice"`
}

func (c pptxNotesContainerXML) shapes() []pptxNotesShapeXML {
	var out []pptxNotesShapeXML
	out = append(out, c.Shapes...)
	for _, g := range c.Groups {
		out = append(out, g.shapes()...)
	}
	for _, alt := range c.Alternates {
		out = append(out, alt.shapes()...)
	}
	return out
}

func (g pptxNotesGroupXML) shapes() []pptxNotesShapeXML {
	var out []pptxNotesShapeXML
	out = append(out, g.Shapes...)
	for _, nested := range g.Groups {
		out = append(out, nested.shapes()...)
	}
	for _, alt := range g.Alternates {
		out = append(out, alt.shapes()...)
	}
	return out
}

type pptxNotesShapeXML struct {
	Ph     *pptxPlaceholderXML `xml:"nvSpPr>nvPr>ph"`
	TxBody *pptxTextBodyXML    `xml:"txBody"`
}

type pptxPlaceholderXML struct {
	Type string `xml:"type,attr"`
}

type pptxTextBodyXML struct {
	Paragraphs []pptxNotesParagraphXML `xml:"p"`
}

type pptxNotesParagraphXML struct {
	Children []pptxNotesRunXML `xml:",any"`
}

type pptxNotesRunXML struct {
	XMLName xml.Name
	Text    string `xml:"t"`
}

// text renders one note paragraph: text runs and fields contribute their
// text in order, hard line breaks (<a:br/>) become newlines.
func (p pptxNotesParagraphXML) text() string {
	var b strings.Builder
	for _, child := range p.Children {
		switch child.XMLName.Local {
		case "br":
			b.WriteByte('\n')
		case "r", "fld":
			b.WriteString(child.Text)
		}
	}
	return b.String()
}
