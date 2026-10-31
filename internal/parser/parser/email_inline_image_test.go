//
// Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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
	"mime/quotedprintable"
	"strings"
	"testing"

	"ragflow/internal/entity"
	"ragflow/internal/utility"
)

// emailImageItems collects the image DocItems emitted for an email.
func emailImageItems(t *testing.T, res ParseResult) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, it := range res.JSON {
		m := it.ToMap()
		if m[DocTypeKey] == DocTypeImage {
			out = append(out, m)
		}
	}
	return out
}

// parseCIDImageRefs is a tiny helper exposing cidImageRefs to tests.
func parseCIDImageRefs(source string) []emailCIDRef { return cidImageRefs(source) }

// relatedAlternativeEML builds the canonical Outlook/Gmail shape: an outer
// multipart/related wrapping a multipart/alternative (plain + HTML) and an
// inline image part addressed by Content-ID.
func relatedAlternativeEML(imageCTE string, imageBody string) []byte {
	raw := strings.Join([]string{
		"From: sender@example.com",
		"To: recipient@example.com",
		"Subject: diagrams",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		`Content-Type: multipart/alternative; boundary="ALT"`,
		"",
		"--ALT",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Plain body with the figure.",
		"--ALT",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<html><body><p>HTML-only wording</p><img alt="The diagram" src="cid:fig1"></body></html>`,
		"--ALT--",
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <fig1>",
		"Content-Transfer-Encoding: " + imageCTE,
		"",
		imageBody,
		"--REL--",
		"",
	}, "\r\n")
	return []byte(raw)
}

func TestEmailParser_RelatedCIDImageExtracted(t *testing.T) {
	png := []byte("PNG-FIGURE-ONE")
	ctx, cache := recordingMediaContext()
	p := NewEmailParser()

	res := p.ParseWithResult(ctx, "figures.eml",
		relatedAlternativeEML("base64", base64.StdEncoding.EncodeToString(png)))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}

	content := res.JSON[0].ToMap()
	if text, _ := content["text"].(string); text != "Plain body with the figure." {
		t.Errorf("text = %q, want the plain-text part only", text)
	}
	if strings.Contains(content["text"].(string), "HTML-only wording") {
		t.Errorf("HTML visible text duplicated into body text: %q", content["text"])
	}
	// The original HTML (cid reference included) is preserved verbatim.
	htmlBody, _ := content["text_html"].(string)
	if !strings.Contains(htmlBody, `src="cid:fig1"`) {
		t.Errorf("text_html lost the cid markup: %q", htmlBody)
	}

	images := emailImageItems(t, res)
	if len(images) != 1 {
		t.Fatalf("image items = %d, want 1 (full items: %d)", len(images), len(res.JSON))
	}
	item := images[0]
	if item["text"] != "The diagram" {
		t.Errorf("image alt text = %v, want \"The diagram\"", item["text"])
	}
	if _, ok := item["image"]; ok {
		t.Errorf("ref-only image must not carry inline bytes: %#v", item)
	}
	if item["media_order"] != 1 {
		t.Errorf("media_order = %v, want 1", item["media_order"])
	}
	ref := entity.MediaRefFromAny(item["media_ref"])
	if ref == nil || ref.Kind != entity.MediaKindKV || ref.Scope != "run-1" {
		t.Fatalf("media_ref = %#v", item["media_ref"])
	}
	if ref.Format != "email" || ref.MIME != "image/png" || ref.Size != int64(len(png)) {
		t.Errorf("media ref = %+v", ref)
	}
	if cache.putCalls != 1 {
		t.Fatalf("cache Put calls = %d, want 1", cache.putCalls)
	}
	if got, err := cache.Get(ctx, *ref); err != nil || !bytes.Equal(got, png) {
		t.Errorf("cached payload = %q, err=%v, want %q", got, err, png)
	}
}

func TestEmailParser_RelatedCIDImageQuotedPrintable(t *testing.T) {
	png := []byte("PNG-FIGURE-QP=\r\n")
	var encoded bytes.Buffer
	w := quotedprintable.NewWriter(&encoded)
	if _, err := w.Write(png); err != nil {
		t.Fatal(err)
	}
	w.Close()

	ctx, cache := recordingMediaContext()
	res := NewEmailParser().ParseWithResult(ctx, "figures.eml",
		relatedAlternativeEML("quoted-printable", encoded.String()))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	images := emailImageItems(t, res)
	if len(images) != 1 {
		t.Fatalf("image items = %d, want 1: warnings=%v", len(images), res.Warnings)
	}
	ref := entity.MediaRefFromAny(images[0]["media_ref"])
	if got, err := cache.Get(ctx, *ref); err != nil || !bytes.Equal(got, png) {
		t.Errorf("cached QP payload = %q, err=%v, want %q", got, err, png)
	}
}

// Two related groups may reuse the same Content-ID; each group's HTML must
// resolve to its own picture.
func TestEmailParser_TwoRelatedGroupsSameContentID(t *testing.T) {
	first := []byte("GROUP-ONE-IMAGE")
	second := []byte("GROUP-TWO-DIFFERENT-IMAGE")
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/mixed; boundary="MIX"`,
		"",
		"--MIX",
		`Content-Type: multipart/related; boundary="R1"`,
		"",
		"--R1",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img alt="one" src="cid:fig1">`,
		"--R1",
		"Content-Type: image/png",
		"Content-ID: <fig1>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(first),
		"--R1--",
		"",
		"--MIX",
		`Content-Type: multipart/related; boundary="R2"`,
		"",
		"--R2",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img alt="two" src="cid:fig1">`,
		"--R2",
		"Content-Type: image/gif",
		"Content-ID: <fig1>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(second),
		"--R2--",
		"",
		"--MIX--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	res := NewEmailParser().ParseWithResult(ctx, "two.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	images := emailImageItems(t, res)
	if len(images) != 2 {
		t.Fatalf("image items = %d, want 2", len(images))
	}
	ref1 := entity.MediaRefFromAny(images[0]["media_ref"])
	ref2 := entity.MediaRefFromAny(images[1]["media_ref"])
	if ref1 == nil || ref2 == nil || ref1.SHA256 == ref2.SHA256 {
		t.Fatalf("refs must differ: %+v vs %+v", ref1, ref2)
	}
	if images[0]["text"] != "one" || images[1]["text"] != "two" {
		t.Errorf("alt text order = %q, %q", images[0]["text"], images[1]["text"])
	}
	got1, err := cache.Get(ctx, *ref1)
	if err != nil || !bytes.Equal(got1, first) {
		t.Errorf("group 1 payload = %q, err=%v, want %q", got1, err, first)
	}
	got2, err := cache.Get(ctx, *ref2)
	if err != nil || !bytes.Equal(got2, second) {
		t.Errorf("group 2 payload = %q, err=%v, want %q", got2, err, second)
	}
}

// A missing cid part is skipped with a parse warning while the rest of the
// message keeps parsing.
func TestEmailParser_MissingCIDReferenceWarnsAndContinues(t *testing.T) {
	png := []byte("PNG-PRESENT")
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		"Content-Type: multipart/alternative; boundary=\"ALT\"",
		"",
		"--ALT",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"body survives",
		"--ALT",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img src="cid:ghost"><p>text</p><img alt="here" src="cid:present">`,
		"--ALT--",
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <present>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(png),
		"--REL--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	res := NewEmailParser().ParseWithResult(ctx, "missing.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if images := emailImageItems(t, res); len(images) != 1 {
		t.Fatalf("image items = %d, want only the present part", len(images))
	}
	if cache.putCalls != 1 {
		t.Errorf("cache Put calls = %d, want 1", cache.putCalls)
	}
	if res.JSON[0].ToMap()["text"] != "body survives" {
		t.Errorf("plain body lost: %v", res.JSON[0].ToMap()["text"])
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "1 inline image reference") {
		t.Fatalf("warnings = %v, want one missing-reference warning", res.Warnings)
	}
}

// A referenced part whose payload cannot be decoded is skipped with a
// warning; valid references in the same message still come through.
func TestEmailParser_CorruptPayloadWarnsAndContinues(t *testing.T) {
	good := []byte("GOOD-IMAGE")
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img alt="broken" src="cid:broken"><img alt="good" src="cid:good">`,
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <broken>",
		"Content-Transfer-Encoding: base64",
		"",
		"@@not-base64@@",
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <good>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(good),
		"--REL--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	res := NewEmailParser().ParseWithResult(ctx, "corrupt.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	images := emailImageItems(t, res)
	if len(images) != 1 || images[0]["text"] != "good" {
		t.Fatalf("image items = %#v, want only the valid image", images)
	}
	if cache.putCalls != 1 {
		t.Errorf("cache Put calls = %d, want 1", cache.putCalls)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "corrupt or empty payload") {
		t.Fatalf("warnings = %v, want one corrupt-payload warning", res.Warnings)
	}
}

// A referenced image over the configured per-image media limit is skipped
// with a warning and never written to the cache.
func TestEmailParser_OversizedInlineImageSkipped(t *testing.T) {
	t.Setenv("RAGFLOW_MEDIA_IMAGE_MAX_BYTES", "16")
	oversized := bytes.Repeat([]byte("x"), 64)
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img src="cid:big">`,
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <big>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(oversized),
		"--REL--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	res := NewEmailParser().ParseWithResult(ctx, "big.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if images := emailImageItems(t, res); len(images) != 0 {
		t.Fatalf("oversized image emitted: %#v", images)
	}
	if cache.putCalls != 0 {
		t.Errorf("cache Put calls = %d, want 0", cache.putCalls)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "16-byte per-image limit") {
		t.Fatalf("warnings = %v, want the per-image-limit warning", res.Warnings)
	}
}

// External http(s) and relative sources are never fetched or emitted; only
// the cid reference is extracted.
func TestEmailParser_ExternalImagesIgnored(t *testing.T) {
	png := []byte("CID-ONLY")
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img src="https://example.com/tracker.png">` +
			`<img src="http://example.com/banner.png">` +
			`<img src="relative/fig.png">` +
			`<img src="//cdn.example.com/protocol-relative.png">` +
			`<img alt="local" src="cid:local">`,
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <local>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(png),
		"--REL--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	res := NewEmailParser().ParseWithResult(ctx, "external.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	images := emailImageItems(t, res)
	if len(images) != 1 || images[0]["text"] != "local" {
		t.Fatalf("image items = %#v, want only the cid image", images)
	}
	if cache.putCalls != 1 {
		t.Errorf("cache Put calls = %d, want 1", cache.putCalls)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("external URLs must not warn as missing cids: %v", res.Warnings)
	}
}

// An inline part the HTML never references is not extracted.
func TestEmailParser_UnreferencedInlineImageIgnored(t *testing.T) {
	png := []byte("UNUSED")
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<p>no images referenced</p>`,
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <orphan>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(png),
		"--REL--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	res := NewEmailParser().ParseWithResult(ctx, "orphan.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if images := emailImageItems(t, res); len(images) != 0 {
		t.Fatalf("unreferenced image emitted: %#v", images)
	}
	if cache.putCalls != 0 {
		t.Errorf("cache Put calls = %d, want 0", cache.putCalls)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
}

// Body-image extraction is gated by the "body" field alone: an
// attachments-only configuration neither walks cid references nor writes the
// cache, while still collecting the regular attachment.
func TestEmailParser_AttachmentsOnlySkipsInlineImages(t *testing.T) {
	png := []byte("INLINE")
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/mixed; boundary="MIX"`,
		"",
		"--MIX",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img src="cid:inline">`,
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <inline>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(png),
		"--REL--",
		"",
		"--MIX",
		"Content-Type: text/plain",
		"Content-Disposition: attachment; filename=\"note.txt\"",
		"",
		"attachment note",
		"--MIX--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	content, images, warnings, err := parseEMLWithError(ctx,
		strings.NewReader(string(raw)), []string{"attachments"}, false)
	if err != nil {
		t.Fatalf("parseEMLWithError: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("inline images = %d, want 0 without the body field", len(images))
	}
	if cache.putCalls != 0 {
		t.Errorf("cache Put calls = %d, want 0", cache.putCalls)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if _, ok := content["text"]; ok {
		t.Errorf("text must be absent without the body field: %#v", content)
	}
	atts, _ := content["attachments"].([]map[string]any)
	if len(atts) != 1 || atts[0]["filename"] != "note.txt" {
		t.Fatalf("attachments = %#v", content["attachments"])
	}
}

// The text-only parser (previews, attachments, CLI) never extracts body
// images, even with a run media cache present.
func TestEmailParser_TextOnlyEntrySkipsInlineImages(t *testing.T) {
	png := []byte("INLINE")
	ctx, cache := recordingMediaContext()
	fp, err := GetTextOnlyParser(utility.FileTypeEMAIL)
	if err != nil {
		t.Fatal(err)
	}
	res := fp.ParseWithResult(ctx, "figures.eml",
		relatedAlternativeEML("base64", base64.StdEncoding.EncodeToString(png)))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if images := emailImageItems(t, res); len(images) != 0 {
		t.Fatalf("text-only parser emitted images: %#v", images)
	}
	if cache.putCalls != 0 {
		t.Errorf("cache Put calls = %d, want 0", cache.putCalls)
	}
	if res.JSON[0].ToMap()["text"] != "Plain body with the figure." {
		t.Errorf("plain body must still extract: %v", res.JSON[0].ToMap()["text"])
	}
}

// A full (ingestion) parse that references an inline image without an
// injected run media cache fails loudly instead of silently dropping the
// image, matching the HTML/EPUB media contract.
func TestEmailParser_MissingMediaCacheFails(t *testing.T) {
	png := []byte("INLINE")
	res := NewEmailParser().ParseWithResult(context.Background(), "figures.eml",
		relatedAlternativeEML("base64", base64.StdEncoding.EncodeToString(png)))
	if res.Err == nil || !strings.Contains(res.Err.Error(), "media cache is not configured") {
		t.Fatalf("Err = %v, want a not-configured media cache error", res.Err)
	}
}

// An image part marked Content-Disposition: attachment stays a regular
// attachment even when it also carries a Content-ID inside the related group.
func TestEmailParser_AttachmentWithContentIDNotInline(t *testing.T) {
	inline := []byte("INLINE")
	attached := []byte("ATTACHED")
	raw := strings.Join([]string{
		"From: sender@example.com",
		`Content-Type: multipart/related; boundary="REL"`,
		"",
		"--REL",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<img src="cid:inline">`,
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <inline>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(inline),
		"--REL",
		"Content-Type: image/png",
		"Content-ID: <attached>",
		"Content-Disposition: attachment; filename=\"diagram.png\"",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(attached),
		"--REL--",
		"",
	}, "\r\n")

	ctx, cache := recordingMediaContext()
	content, images, _, err := parseEMLWithError(ctx,
		strings.NewReader(string(raw)),
		[]string{"from", "body", "attachments"}, false)
	if err != nil {
		t.Fatalf("parseEMLWithError: %v", err)
	}
	atts, _ := content["attachments"].([]map[string]any)
	if len(atts) != 1 || atts[0]["filename"] != "diagram.png" {
		t.Fatalf("attachments = %#v", content["attachments"])
	}
	if len(images) != 1 {
		t.Fatalf("inline image items = %d, want 1", len(images))
	}
	ref := entity.MediaRefFromAny(images[0]["media_ref"])
	if ref == nil || ref.Size != int64(len(inline)) {
		t.Fatalf("inline ref = %+v, want the inline part", ref)
	}
	if cache.putCalls != 1 {
		t.Errorf("cache Put calls = %d, want 1 (the attachment is not cached)", cache.putCalls)
	}

	// End to end, the attachment must not surface as a body image item
	// either: image re-chunking skips visual attachments, so only the one
	// referenced inline image remains.
	res := NewEmailParser().ParseWithResult(ctx, "mixed.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if got := emailImageItems(t, res); len(got) != 1 {
		t.Fatalf("end-to-end image items = %d, want 1", len(got))
	}
}

// A related group nested as an email attachment is parsed text-only: its
// body is re-chunked but its cid image needs no run media cache.
func TestEmailParser_NestedEmailRechunkTextOnly(t *testing.T) {
	inner := relatedAlternativeEML("base64", base64.StdEncoding.EncodeToString([]byte("NESTED-IMG")))
	raw := strings.Join([]string{
		"From: outer@example.com",
		`Content-Type: multipart/mixed; boundary="MIX"`,
		"",
		"--MIX",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"outer body",
		"--MIX",
		"Content-Type: message/rfc822",
		"Content-Disposition: attachment; filename=\"nested.eml\"",
		"",
		string(inner),
		"--MIX--",
		"",
	}, "\r\n")

	// No media cache on purpose: the nested parser is text-only and must not
	// require one.
	res := NewEmailParser().ParseWithResult(context.Background(), "outer.eml", []byte(raw))
	if res.Err != nil {
		t.Fatalf("ParseWithResult: %v", res.Err)
	}
	if images := emailImageItems(t, res); len(images) != 0 {
		t.Fatalf("nested email emitted inline images: %#v", images)
	}
	foundNested := false
	for _, it := range res.JSON {
		if s, ok := it.ToMap()["text"].(string); ok && strings.Contains(s, "Plain body with the figure.") {
			foundNested = true
		}
	}
	if !foundNested {
		t.Fatalf("nested email body not re-chunked: %#v", res.JSON)
	}
}

func TestParseCIDURL(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
		ok   bool
	}{
		{"plain", "cid:fig1", "fig1", true},
		{"uppercase scheme", "CID:fig1", "fig1", true},
		{"surrounding spaces", "  cid:fig1  ", "fig1", true},
		{"angle brackets", "cid:<fig1@example.com>", "fig1@example.com", true},
		{"percent escape", "cid:ima%24ge", "ima$ge", true},
		{"http url", "https://example.com/a.png", "", false},
		{"relative", "a.png", "", false},
		{"empty id", "cid:  ", "", false},
		{"data uri", "data:image/png;base64,AAAA", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCIDURL(tt.src)
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseCIDURL(%q) = %q,%v want %q,%v", tt.src, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestEmailContentID(t *testing.T) {
	tests := map[string]string{
		"<fig1@example.com>": "fig1@example.com",
		"  <fig1> ":          "fig1",
		"bare-id":            "bare-id",
		"":                   "",
	}
	for in, want := range tests {
		if got := emailContentID(in); got != want {
			t.Errorf("emailContentID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCIDImageRefs_DocumentOrderAndAlt(t *testing.T) {
	htmlText := `<img alt="A" src="cid:a"><img src="https://x/y.png">` +
		`<table><tr><td><img alt='B' src="cid:b"></td></tr></table>` +
		`<img alt="A again" src="cid:a">`
	refs := parseCIDImageRefs(htmlText)
	got := make([][2]string, 0, len(refs))
	for _, ref := range refs {
		got = append(got, [2]string{ref.cid, ref.alt})
	}
	want := [][2]string{{"a", "A"}, {"b", "B"}, {"a", "A again"}}
	if len(got) != len(want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ref %d = %v, want %v", i, got[i], want[i])
		}
	}
}
