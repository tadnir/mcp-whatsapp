package mcp

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// testChatJID and testMessageID name the message every test downloads.
const (
	testChatJID   = "972500000000@s.whatsapp.net"
	testMessageID = "3EB0ABCDEF"
)

// jpegMagic is the minimum prefix (SOI + APP0 marker) that
// http.DetectContentType recognises as image/jpeg.
var jpegMagic = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}

// oggMagic is the "OggS" capture pattern for an Ogg container. The
// http.DetectContentType sniff table matches this even without a Vorbis /
// Opus payload following it.
var oggMagic = []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00")

// writeFile writes bytes to a fresh path under t.TempDir() and returns the
// absolute path. Fatals the test on any I/O error.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// firstText returns the Text field of the first content block, which the
// downloadMediaResult contract guarantees is a JSON summary.
func firstText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty content")
	}
	txt, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] should be TextContent, got %T", res.Content[0])
	}
	return txt.Text
}

// assertJSONSummary parses the first (text) content block and checks the
// key fields are what we expected. Any mismatch fatals — this is the
// backwards-compat contract with pre-inline callers.
func assertJSONSummary(t *testing.T, res *mcp.CallToolResult, want client.DownloadResult) {
	t.Helper()
	var got client.DownloadResult
	if err := json.Unmarshal([]byte(firstText(t, res)), &got); err != nil {
		t.Fatalf("summary is not JSON: %v (raw: %q)", err, firstText(t, res))
	}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

// TestDownloadMediaResult_ImageInlined: a successful image download whose
// file is on disk and under the size cap should yield two content blocks
// — the JSON summary and an ImageContent with base64 bytes.
func TestDownloadMediaResult_ImageInlined(t *testing.T) {
	path := writeFile(t, "img.jpg", jpegMagic)
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded image media",
		MediaType: "image",
		Filename:  "img.jpg",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	if len(res.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(res.Content))
	}
	assertJSONSummary(t, res, r)

	img, ok := res.Content[1].(mcp.ImageContent)
	if !ok {
		t.Fatalf("content[1] should be ImageContent, got %T", res.Content[1])
	}
	if img.MIMEType != "image/jpeg" {
		t.Errorf("MIMEType = %q, want image/jpeg", img.MIMEType)
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("image data is not base64: %v", err)
	}
	if string(decoded) != string(jpegMagic) {
		t.Errorf("decoded bytes = %x, want %x", decoded, jpegMagic)
	}
}

// TestDownloadMediaResult_AudioInlined: successful audio download inlines
// bytes as AudioContent with the sniffed MIME.
func TestDownloadMediaResult_AudioInlined(t *testing.T) {
	path := writeFile(t, "voice.ogg", oggMagic)
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded audio media",
		MediaType: "audio",
		Filename:  "voice.ogg",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	if len(res.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(res.Content))
	}
	aud, ok := res.Content[1].(mcp.AudioContent)
	if !ok {
		t.Fatalf("content[1] should be AudioContent, got %T", res.Content[1])
	}
	if !strings.HasPrefix(aud.MIMEType, "audio/") {
		t.Errorf("MIMEType = %q, want audio/*", aud.MIMEType)
	}
}

// embeddedBlob returns the blob of the EmbeddedResource in content[1].
func embeddedBlob(t *testing.T, res *mcp.CallToolResult) mcp.BlobResourceContents {
	t.Helper()
	if len(res.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(res.Content))
	}
	emb, ok := res.Content[1].(mcp.EmbeddedResource)
	if !ok {
		t.Fatalf("content[1] should be EmbeddedResource, got %T", res.Content[1])
	}
	blob, ok := emb.Resource.(mcp.BlobResourceContents)
	if !ok {
		t.Fatalf("embedded resource should be a blob, got %T", emb.Resource)
	}
	return blob
}

// summaryOf parses the JSON summary into the full downloadMediaSummary.
func summaryOf(t *testing.T, res *mcp.CallToolResult) downloadMediaSummary {
	t.Helper()
	var got downloadMediaSummary
	if err := json.Unmarshal([]byte(firstText(t, res)), &got); err != nil {
		t.Fatalf("summary is not JSON: %v", err)
	}
	return got
}

// TestDownloadMediaResult_DocumentEmbedded: a document under the cap comes
// back as an EmbeddedResource blob with its sniffed MIME type and its
// whatsapp://media URI, so tool-only remote clients get the file.
func TestDownloadMediaResult_DocumentEmbedded(t *testing.T) {
	pdf := []byte("%PDF-1.4\n%test")
	path := writeFile(t, "spec.pdf", pdf)
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded document media",
		MediaType: "document",
		Filename:  "spec.pdf",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	assertJSONSummary(t, res, r)
	blob := embeddedBlob(t, res)
	if blob.MIMEType != "application/pdf" {
		t.Errorf("MIMEType = %q, want application/pdf", blob.MIMEType)
	}
	decoded, err := base64.StdEncoding.DecodeString(blob.Blob)
	if err != nil || string(decoded) != string(pdf) {
		t.Errorf("blob does not decode to the file (err %v)", err)
	}
	chat, msg, err := parseMediaURI(blob.URI)
	if err != nil || chat != testChatJID || msg != testMessageID {
		t.Errorf("URI %q parses to (%q, %q, %v)", blob.URI, chat, msg, err)
	}
	sum := summaryOf(t, res)
	if !sum.Embedded || sum.ResourceURI != blob.URI || sum.Size != int64(len(pdf)) || sum.MIMEType != "application/pdf" {
		t.Errorf("summary = %+v", sum)
	}
}

// TestDownloadMediaResult_VideoEmbedded: same as document.
func TestDownloadMediaResult_VideoEmbedded(t *testing.T) {
	path := writeFile(t, "clip.mp4", []byte{0x00, 0x00, 0x00, 0x0C, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'})
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded video media",
		MediaType: "video",
		Filename:  "clip.mp4",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	if blob := embeddedBlob(t, res); blob.MIMEType != "video/mp4" {
		t.Errorf("MIMEType = %q, want video/mp4", blob.MIMEType)
	}
}

// TestDownloadMediaResult_MIMEFromExtension: when sniffing finds only
// octet-stream, the filename's extension names the type.
func TestDownloadMediaResult_MIMEFromExtension(t *testing.T) {
	path := writeFile(t, "notes.json", []byte{0x00, 0x01, 0x02})
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded document media",
		MediaType: "document",
		Filename:  "notes.json",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	if blob := embeddedBlob(t, res); !strings.HasPrefix(blob.MIMEType, "application/json") {
		t.Errorf("MIMEType = %q, want application/json", blob.MIMEType)
	}
}

// TestDownloadMediaResult_OversizedDocumentLinked: a document over the
// resource cap is not embedded; a ResourceLink points at resources/read.
func TestDownloadMediaResult_OversizedDocumentLinked(t *testing.T) {
	big := make([]byte, inlineResourceMaxBytes+1)
	copy(big, "%PDF-1.4")
	path := writeFile(t, "big.pdf", big)
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded document media",
		MediaType: "document",
		Filename:  "big.pdf",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	assertLinked(t, res, "big.pdf")
	if sum := summaryOf(t, res); sum.Embedded || sum.Size != int64(len(big)) {
		t.Errorf("summary = %+v", sum)
	}
}

// TestDownloadMediaResult_IncludeContentFalse: with include_content false,
// even a small image comes back as a link only.
func TestDownloadMediaResult_IncludeContentFalse(t *testing.T) {
	path := writeFile(t, "img.jpg", jpegMagic)
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded image media",
		MediaType: "image",
		Filename:  "img.jpg",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, false)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	assertLinked(t, res, "img.jpg")
}

// assertLinked checks content[1] is a ResourceLink to the test message.
func assertLinked(t *testing.T, res *mcp.CallToolResult, name string) {
	t.Helper()
	if len(res.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(res.Content))
	}
	link, ok := res.Content[1].(mcp.ResourceLink)
	if !ok {
		t.Fatalf("content[1] should be ResourceLink, got %T", res.Content[1])
	}
	if link.URI != mediaResourceURI(testChatJID, testMessageID) || link.Name != name {
		t.Errorf("link = %+v", link)
	}
}

// TestDownloadMediaResult_FailurePathOnly: a failed download still returns
// a well-formed CallToolResult with the JSON error, and no inline attempt
// is made (there's no file to read).
func TestDownloadMediaResult_FailurePathOnly(t *testing.T) {
	r := client.DownloadResult{
		Success: false,
		Message: "failed to download media: download failed with status code 403",
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected 1 content block on failure, got %d", len(res.Content))
	}
	assertJSONSummary(t, res, r)
}

// TestDownloadMediaResult_MissingFileFallsBack: if the daemon says success
// but the file has vanished (unlikely race), the tool still returns the
// JSON summary rather than propagating the read error.
func TestDownloadMediaResult_MissingFileFallsBack(t *testing.T) {
	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded image media",
		MediaType: "image",
		Filename:  "gone.jpg",
		Path:      filepath.Join(t.TempDir(), "does-not-exist.jpg"),
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected 1 content block when file is missing, got %d", len(res.Content))
	}
}

// TestDownloadMediaResult_OversizedNotInlined: images above the inline cap
// are NOT embedded; a ResourceLink points at resources/read instead. This protects the
// transport frame and the LLM context from multi-megabyte payloads.
func TestDownloadMediaResult_OversizedNotInlined(t *testing.T) {
	// One byte over the cap. Prefix with jpegMagic so http.DetectContentType
	// would return image/jpeg if we did read it — proving the size check is
	// what suppresses embedding, not the sniffer.
	oversized := make([]byte, inlineMediaMaxBytes+1)
	copy(oversized, jpegMagic)
	path := writeFile(t, "huge.jpg", oversized)

	r := client.DownloadResult{
		Success:   true,
		Message:   "Successfully downloaded image media",
		MediaType: "image",
		Filename:  "huge.jpg",
		Path:      path,
	}

	res, err := downloadMediaResult(r, testChatJID, testMessageID, true)
	if err != nil {
		t.Fatalf("downloadMediaResult: %v", err)
	}
	assertLinked(t, res, "huge.jpg")
}
