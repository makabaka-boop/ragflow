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
	"context"
	"strconv"
	"strings"
	"testing"
)

// --- synthetic OOXML package fixtures -------------------------------------

const (
	pptxNSA   = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`
	pptxNSP   = `xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`
	pptxNSR   = `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	pptxNSRel = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
)

func pptxZipBytes(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func fixturePresentationXML(rids ...string) string {
	var ids strings.Builder
	for _, rid := range rids {
		ids.WriteString(`<p:sldId id="256" r:id="` + rid + `"/>`)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<p:presentation ` + pptxNSA + ` ` + pptxNSR + ` ` + pptxNSP + `>` +
		`<p:sldMasterIdLst/>` +
		`<p:sldIdLst>` + ids.String() + `</p:sldIdLst>` +
		`</p:presentation>`
}

func fixturePresentationRelsXML(targets map[string]string) string {
	var b strings.Builder
	for rid, target := range targets {
		b.WriteString(`<Relationship Id="` + rid + `" Type="` + pptxRelSlide + `" Target="` + target + `"/>`)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships ` + pptxNSRel + `>` + b.String() + `</Relationships>`
}

// pptxSlideRelsXML builds a slide rels part. notesTarget == "" means the
// slide has no notesSlide relationship.
func fixtureSlideRelsXML(notesTarget string) string {
	var b strings.Builder
	b.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>`)
	if notesTarget != "" {
		b.WriteString(`<Relationship Id="rId2" Type="` + pptxRelNotesSlide + `" Target="` + notesTarget + `"/>`)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships ` + pptxNSRel + `>` + b.String() + `</Relationships>`
}

// pptxNotesSlideXML builds a notesSlide part carrying the note paragraphs in
// the body placeholder plus the slide thumbnail picture and sldNum/date/
// footer placeholders that notes extraction must exclude. Paragraphs may use
// "\n" to encode a hard line break inside one paragraph.
func fixtureNotesSlideXML(paragraphs ...string) string {
	var body strings.Builder
	shape := func(id, name, ph, txBody string) string {
		return `<p:sp><p:nvSpPr><p:cNvPr id="` + id + `" name="` + name + `"/>` +
			`<p:cNvSpPr/><p:nvPr><p:ph type="` + ph + `"/></p:nvPr></p:nvSpPr>` +
			`<p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/>` + txBody +
			`</p:txBody></p:sp>`
	}
	para := func(text string) string {
		var runs strings.Builder
		runs.WriteString(`<a:p><a:pPr/>`)
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if i > 0 {
				runs.WriteString(`<a:br/>`)
			}
			runs.WriteString(`<a:r><a:rPr lang="en-US"/><a:t>` + line + `</a:t></a:r>`)
		}
		runs.WriteString(`</a:p>`)
		return runs.String()
	}
	for _, p := range paragraphs {
		body.WriteString(para(p))
	}
	thumbnail := `<p:pic><p:nvPicPr><p:cNvPr id="9" name="Presentation Image"/><p:cNvPicPr/><p:nvPr/></p:nvPicPr>` +
		`<p:blipFill><a:blip r:embed="rIdThumb"/><a:stretch><a:fillRect/></a:stretch></p:blipFill>` +
		`<p:spPr/></p:pic>`
	sldNum := shape("4", "Slide Number Placeholder", "sldNum",
		`<a:p><a:fld id="f1" type="slidenum"><a:rPr/><a:t>‹#›</a:t></a:fld></a:p>`)
	date := shape("5", "Date Placeholder", "dt",
		`<a:p><a:fld id="f2" type="datetime"><a:rPr/><a:t>2026-01-01</a:t></a:fld></a:p>`)
	footer := shape("6", "Footer Placeholder", "ftr",
		`<a:p><a:r><a:t>Confidential</a:t></a:r></a:p>`)
	header := shape("7", "Header Placeholder", "hdr",
		`<a:p><a:r><a:t>Page header text</a:t></a:r></a:p>`)
	notes := shape("3", "Notes Placeholder", "body", body.String())
	// The image placeholder is a shape with ph type pic, not a text body.
	imagePH := shape("2", "Slide Image Placeholder", "pic", "")
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<p:notes ` + pptxNSA + ` ` + pptxNSR + ` ` + pptxNSP + `><p:cSld><p:spTree>` +
		`<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>` +
		`<p:grpSpPr/>` +
		imagePH + thumbnail + notes + sldNum + date + footer + header +
		`</p:spTree></p:cSld></p:notes>`
}

// threeSlideDeck builds a healthy 3-slide deck whose notes part file names
// deliberately disagree with presentation order.
func threeSlideDeck() map[string]string {
	parts := map[string]string{
		"ppt/presentation.xml": fixturePresentationXML("rId2", "rId3", "rId4"),
		"ppt/_rels/presentation.xml.rels": fixturePresentationRelsXML(map[string]string{
			"rId2": "slides/slide1.xml",
			"rId3": "slides/slide2.xml",
			"rId4": "slides/slide3.xml",
		}),
		"ppt/slides/_rels/slide1.xml.rels": fixtureSlideRelsXML("../notesSlides/notesSlide9.xml"),
		"ppt/slides/_rels/slide2.xml.rels": fixtureSlideRelsXML("../notesSlides/notesSlide7.xml"),
		"ppt/slides/_rels/slide3.xml.rels": fixtureSlideRelsXML("../notesSlides/notesSlide8.xml"),
		"ppt/notesSlides/notesSlide9.xml": fixtureNotesSlideXML(
			"notes for slide one", "second paragraph\nwith hard break"),
		"ppt/notesSlides/notesSlide7.xml": fixtureNotesSlideXML("notes for slide two"),
		"ppt/notesSlides/notesSlide8.xml": fixtureNotesSlideXML("notes for slide three", ""),
	}
	for i := 1; i <= 3; i++ {
		parts["ppt/slides/slide"+strconv.Itoa(i)+".xml"] = "<p:sld/>"
	}
	return parts
}

func notesBySlide(notes []pptxSlideNotes) map[int]string {
	out := map[int]string{}
	for _, n := range notes {
		out[n.slideNumber] = n.text
	}
	return out
}

// --- extractor tests ------------------------------------------------------

func TestExtractPPTXSpeakerNotes_PresentationOrder(t *testing.T) {
	data := pptxZipBytes(t, threeSlideDeck())
	notes, warnings := extractPPTXSpeakerNotes(data)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	got := notesBySlide(notes)
	if got[1] != "notes for slide one\nsecond paragraph\nwith hard break" {
		t.Errorf("slide 1 notes = %q", got[1])
	}
	if got[2] != "notes for slide two" {
		t.Errorf("slide 2 notes = %q", got[2])
	}
	if got[3] != "notes for slide three" {
		t.Errorf("slide 3 notes = %q", got[3])
	}
	// Trailing empty paragraph must not add content.
	if strings.HasSuffix(got[1], "\n") || strings.HasSuffix(got[3], "\n") {
		t.Errorf("outer whitespace not trimmed: %q / %q", got[1], got[3])
	}
}

func TestPPTXNotesBodyText_ExcludesPlaceholdersAndThumbnail(t *testing.T) {
	xml := fixtureNotesSlideXML("real note")
	text, err := pptxNotesBodyText([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if text != "real note" {
		t.Fatalf("body text = %q, want only the body placeholder", text)
	}
}

func TestPPTXNotesBodyText_PreservesBlankParagraphs(t *testing.T) {
	// A blank line between note paragraphs is paragraph structure and must
	// survive; a wholly empty body yields no note.
	xml := fixtureNotesSlideXML("first", "", "third")
	text, err := pptxNotesBodyText([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if text != "first\n\nthird" {
		t.Fatalf("text = %q, want preserved blank paragraph", text)
	}

	empty, err := pptxNotesBodyText([]byte(fixtureNotesSlideXML("", "   ")))
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Fatalf("empty body = %q, want empty string", empty)
	}
}

func TestPPTXNotesBodyText_GroupedBodyShape(t *testing.T) {
	// A body placeholder nested inside a group shape is still a note.
	xml := `<?xml version="1.0"?>` +
		`<p:notes ` + pptxNSA + ` ` + pptxNSP + `><p:cSld><p:spTree>` +
		`<p:nvGrpSpPr/>` +
		`<p:grpSp><p:nvGrpSpPr/><p:grpSpPr/>` +
		`<p:sp><p:nvSpPr><p:cNvPr id="2" name="x"/><p:cNvSpPr/><p:nvPr><p:ph type="body"/></p:nvPr></p:nvSpPr>` +
		`<p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>grouped note</a:t></a:r></a:p></p:txBody></p:sp>` +
		`</p:grpSp></p:spTree></p:cSld></p:notes>`
	text, err := pptxNotesBodyText([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if text != "grouped note" {
		t.Fatalf("text = %q", text)
	}
}

func TestExtractPPTXSpeakerNotes_NoNotesAndEmptyNotesAreSilent(t *testing.T) {
	parts := map[string]string{
		"ppt/presentation.xml":             fixturePresentationXML("rId2", "rId3"),
		"ppt/_rels/presentation.xml.rels":  fixturePresentationRelsXML(map[string]string{"rId2": "slides/slide1.xml", "rId3": "slides/slide2.xml"}),
		"ppt/slides/slide1.xml":            "<p:sld/>",
		"ppt/slides/slide2.xml":            "<p:sld/>",
		"ppt/slides/_rels/slide1.xml.rels": fixtureSlideRelsXML("../notesSlides/notesSlide1.xml"),
		"ppt/slides/_rels/slide2.xml.rels": fixtureSlideRelsXML(""), // no rels to a notes part
		"ppt/notesSlides/notesSlide1.xml":  fixtureNotesSlideXML("", "  "),
	}
	notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
	if len(notes) != 0 {
		t.Fatalf("notes = %+v, want none", notes)
	}
	if len(warnings) != 0 {
		t.Fatalf("empty/missing notes must be silent, got %v", warnings)
	}
}

func TestExtractPPTXSpeakerNotes_MissingRelsPartIsSilent(t *testing.T) {
	parts := map[string]string{
		"ppt/presentation.xml":            fixturePresentationXML("rId2"),
		"ppt/_rels/presentation.xml.rels": fixturePresentationRelsXML(map[string]string{"rId2": "slides/slide1.xml"}),
		"ppt/slides/slide1.xml":           "<p:sld/>",
	}
	notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
	if len(notes) != 0 || len(warnings) != 0 {
		t.Fatalf("notes=%v warnings=%v", notes, warnings)
	}
}

func TestExtractPPTXSpeakerNotes_CorruptionWarnsButKeepsGoing(t *testing.T) {
	t.Run("missing notes part", func(t *testing.T) {
		parts := threeSlideDeck()
		delete(parts, "ppt/notesSlides/notesSlide7.xml") // slide 2 target dangling
		notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
		got := notesBySlide(notes)
		if got[1] == "" || got[3] == "" {
			t.Fatalf("readable notes lost: %+v", got)
		}
		if _, ok := got[2]; ok {
			t.Fatalf("slide 2 must have no note: %+v", got)
		}
		if !joinedContains(warnings, "slide 2") || !joinedContains(warnings, "not found") {
			t.Fatalf("want slide 2 missing-part warning, got %v", warnings)
		}
	})

	t.Run("malformed notes xml", func(t *testing.T) {
		parts := threeSlideDeck()
		parts["ppt/notesSlides/notesSlide7.xml"] = "<p:notes><broken"
		notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
		got := notesBySlide(notes)
		if got[1] == "" || got[3] == "" {
			t.Fatalf("readable notes lost: %+v", got)
		}
		if !joinedContains(warnings, "slide 2") {
			t.Fatalf("want slide 2 warning, got %v", warnings)
		}
	})

	t.Run("malformed slide rels xml", func(t *testing.T) {
		parts := threeSlideDeck()
		parts["ppt/slides/_rels/slide2.xml.rels"] = "<Relationships><broken"
		notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
		got := notesBySlide(notes)
		if got[1] == "" || got[3] == "" {
			t.Fatalf("readable notes lost: %+v", got)
		}
		if !joinedContains(warnings, "slide 2") {
			t.Fatalf("want slide 2 warning, got %v", warnings)
		}
	})

	t.Run("missing presentation rel target", func(t *testing.T) {
		parts := threeSlideDeck()
		parts["ppt/_rels/presentation.xml.rels"] = fixturePresentationRelsXML(map[string]string{
			"rId2": "slides/slide1.xml",
			"rId4": "slides/slide3.xml",
		})
		notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
		got := notesBySlide(notes)
		if got[1] == "" || got[3] == "" {
			t.Fatalf("readable notes lost: %+v", got)
		}
		if !joinedContains(warnings, "slide 2") {
			t.Fatalf("want slide 2 relationship warning, got %v", warnings)
		}
	})

	t.Run("missing presentation xml", func(t *testing.T) {
		parts := threeSlideDeck()
		delete(parts, "ppt/presentation.xml")
		notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
		if len(notes) != 0 || !joinedContains(warnings, "presentation.xml") {
			t.Fatalf("notes=%v warnings=%v", notes, warnings)
		}
	})

	t.Run("malformed presentation xml", func(t *testing.T) {
		parts := threeSlideDeck()
		parts["ppt/presentation.xml"] = "<p:presentation><broken"
		notes, warnings := extractPPTXSpeakerNotes(pptxZipBytes(t, parts))
		if len(notes) != 0 || !joinedContains(warnings, "presentation.xml") {
			t.Fatalf("notes=%v warnings=%v", notes, warnings)
		}
	})

	t.Run("not a zip", func(t *testing.T) {
		notes, warnings := extractPPTXSpeakerNotes([]byte("not a package at all"))
		if len(notes) != 0 || !joinedContains(warnings, "OOXML") {
			t.Fatalf("notes=%v warnings=%v", notes, warnings)
		}
	})
}

// --- merge tests ----------------------------------------------------------

func TestAppendPPTXSpeakerNotes_OrderAndIdentity(t *testing.T) {
	textItem := func(slide int, text string) map[string]any {
		return map[string]any{"text": text, "doc_type_kwd": "text", "slide_number": slide}
	}
	tableItem := func(slide int) map[string]any {
		return map[string]any{"doc_type_kwd": "table", "slide_number": slide}
	}
	items := []map[string]any{
		textItem(1, "Body 1"),
		tableItem(1),
		textItem(2, "Body 2"),
		textItem(3, "Body 3"),
	}
	notes := []pptxSlideNotes{
		{slideNumber: 1, text: "Note one"},
		{slideNumber: 2, text: "Note two"},
		{slideNumber: 4, text: "Notes without body"},
	}
	out := appendPPTXSpeakerNotes(items, notes)

	want := []struct {
		slide int
		text  string
		note  bool
	}{
		{1, "Body 1", false},
		{1, "", false}, // table keeps its position before the note
		{1, pptxSpeakerNotesMarker + "\nNote one", true},
		{2, "Body 2", false},
		{2, pptxSpeakerNotesMarker + "\nNote two", true},
		{3, "Body 3", false},
		{4, pptxSpeakerNotesMarker + "\nNotes without body", true},
	}
	if len(out) != len(want) {
		t.Fatalf("items = %d, want %d: %+v", len(out), len(want), out)
	}
	for i, w := range want {
		item := out[i]
		if item["slide_number"] != w.slide {
			t.Errorf("item %d slide_number = %v, want %d", i, item["slide_number"], w.slide)
		}
		if item["doc_type_kwd"] != "text" && !w.note && i != 1 {
			t.Errorf("item %d doc_type_kwd = %v", i, item["doc_type_kwd"])
		}
		if w.note {
			if item["text"] != w.text || item["speaker_notes"] != true {
				t.Errorf("item %d note = %+v, want %q", i, item, w.text)
			}
		}
	}
}

func TestAppendPPTXSpeakerNotes_NoSlideMetadataUnchanged(t *testing.T) {
	items := []map[string]any{{"text": "whole deck", "doc_type_kwd": "text"}}
	out := appendPPTXSpeakerNotes(items, []pptxSlideNotes{{slideNumber: 1, text: "Note"}})
	if len(out) != 1 || out[0]["text"] != "whole deck" {
		t.Fatalf("plain-text salvage items must stay unchanged, got %+v", out)
	}
}

func TestAppendPPTXSpeakerNotes_EmptyNotesNoop(t *testing.T) {
	items := []map[string]any{{"text": "body", "doc_type_kwd": "text", "slide_number": 1}}
	if out := appendPPTXSpeakerNotes(items, nil); len(out) != 1 {
		t.Fatalf("nil notes changed items: %+v", out)
	}
}

// --- configuration --------------------------------------------------------

func TestPPTXParser_ConfigureFromSetup_SpeakerNotes(t *testing.T) {
	p := NewPPTXParser()
	p.ConfigureFromSetup(map[string]any{"extract_speaker_notes": true})
	if !p.ExtractSpeakerNotes {
		t.Fatal("ExtractSpeakerNotes = false, want true")
	}
	p.ConfigureFromSetup(map[string]any{"extract_speaker_notes": false})
	if p.ExtractSpeakerNotes {
		t.Fatal("ExtractSpeakerNotes = true, want false")
	}
	p = NewPPTXParser()
	p.ConfigureFromSetup(map[string]any{"extract_speaker_notes": "yes"})
	if p.ExtractSpeakerNotes {
		t.Fatal("non-bool value must leave the default (off)")
	}
}

// --- end-to-end ParseWithResult (native extraction stubbed) ---------------

func stubPPTXExtract(t *testing.T, irJSON string) {
	t.Helper()
	orig := pptxExtract
	t.Cleanup(func() { pptxExtract = orig })
	pptxExtract = func([]byte, string) (string, string, string, error, error) {
		return irJSON, "", "", nil, nil
	}
}

func TestPPTXParser_ParseWithResult_SpeakerNotes(t *testing.T) {
	parts := threeSlideDeck()
	// Slide 2 carries no body IR elements (blank slide); its note must still
	// survive. Slide 1 keeps a table before the note.
	irJSON := `{"sections":[
		{"elements":[
			{"type":"paragraph","content":[{"type":"text","text":"Body one"}]},
			{"type":"table","rows":[{"cells":[
				{"content":[{"type":"paragraph","content":[{"type":"text","text":"cell"}]}]}
			]}]}
		]},
		{"elements":[]},
		{"elements":[{"type":"paragraph","content":[{"type":"text","text":"Body three"}]}]}
	]}`
	stubPPTXExtract(t, irJSON)

	p := NewPPTXParser()
	p.ExtractSpeakerNotes = true
	res := p.ParseWithResult(context.Background(), "deck.pptx", pptxZipBytes(t, parts))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	maps := ItemsToMaps(res.JSON)

	var texts []string
	for _, m := range maps {
		if text, _ := m["text"].(string); text != "" {
			texts = append(texts, text)
		}
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, pptxSpeakerNotesMarker+"\nnotes for slide one") {
		t.Errorf("slide 1 note missing: %s", joined)
	}
	if !strings.Contains(joined, pptxSpeakerNotesMarker+"\nnotes for slide two") {
		t.Errorf("blank slide 2 lost its note: %s", joined)
	}
	if strings.Contains(joined, "notes for slide three") == false {
		t.Errorf("slide 3 note missing: %s", joined)
	}
	// Every item keeps its slide number, notes included.
	for _, m := range maps {
		if m["slide_number"] == nil {
			t.Errorf("item without slide_number: %+v", m)
		}
	}
	// The note follows the slide 1 table.
	noteIndex, tableIndex := -1, -1
	for i, m := range maps {
		if m["speaker_notes"] == true && strings.Contains(m["text"].(string), "slide one") {
			noteIndex = i
		}
		if m["doc_type_kwd"] == "table" {
			tableIndex = i
		}
	}
	if noteIndex < 0 || tableIndex < 0 || noteIndex < tableIndex {
		t.Fatalf("note must follow the table: table=%d note=%d", tableIndex, noteIndex)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
}

func TestPPTXParser_ParseWithResult_SpeakerNotesOffByDefault(t *testing.T) {
	stubPPTXExtract(t, `{"sections":[
		{"elements":[{"type":"paragraph","content":[{"type":"text","text":"Body one"}]}]}
	]}`)
	res := NewPPTXParser().ParseWithResult(context.Background(), "deck.pptx", pptxZipBytes(t, threeSlideDeck()))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	for _, m := range ItemsToMaps(res.JSON) {
		if m["speaker_notes"] != nil {
			t.Fatalf("notes must be off by default: %+v", m)
		}
	}
}

func joinedContains(xs []string, sub string) bool {
	return strings.Contains(strings.Join(xs, "\n"), sub)
}
