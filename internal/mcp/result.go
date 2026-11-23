package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// resultJSON serialises v to JSON and wraps it in a text tool result.
func resultJSON(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// inlineMediaMaxBytes is the size cap for embedding decrypted image or audio
// bytes directly in a download_media tool result as ImageContent /
// AudioContent, which clients put in the model's context.
const inlineMediaMaxBytes = 5 * 1024 * 1024

// inlineResourceMaxBytes is the size cap for embedding a decrypted video or
// document in a download_media tool result as an EmbeddedResource blob.
// Clients that only use tools (not resources/read) get the file this way.
// Anything larger is returned as a ResourceLink to its whatsapp://media URI.
const inlineResourceMaxBytes = 10 * 1024 * 1024

// downloadMediaSummary is the JSON block download_media returns first. It
// embeds client.DownloadResult, so callers that read content[0].text as a
// DownloadResult keep working; the extra fields say how to get the bytes.
type downloadMediaSummary struct {
	client.DownloadResult
	// ResourceURI reads the file's bytes through resources/read.
	ResourceURI string `json:",omitempty"`
	// MIMEType is the sniffed type of the decrypted file.
	MIMEType string `json:",omitempty"`
	// Size is the decrypted file's size in bytes.
	Size int64 `json:",omitempty"`
	// Embedded is true when the bytes are in the next content block.
	Embedded bool
}

// mediaResourceURI builds the whatsapp://media URI that handleMediaResource
// (resources.go) parses back.
func mediaResourceURI(chatJID, messageID string) string {
	return mediaURIPrefix + url.PathEscape(chatJID) + "/" + messageID
}

// downloadMediaResult builds the CallToolResult returned by the download_media
// tool. The first content block is always the JSON summary (backwards
// compatible with callers that read content[0].text). When the download
// succeeded and includeContent is set, a second block carries the file:
//
//   - images and audio at most inlineMediaMaxBytes as ImageContent /
//     AudioContent, so the model can see or hear them;
//   - videos and documents at most inlineResourceMaxBytes as an
//     EmbeddedResource with the base64 bytes, so a remote client can save
//     or open the file without the daemon's filesystem;
//   - anything larger, or with includeContent unset, as a ResourceLink to
//     the file's whatsapp://media URI, which resources/read serves.
//
// If the file is missing or unreadable, the call still succeeds with just
// the JSON block.
func downloadMediaResult(r client.DownloadResult, chatJID, messageID string, includeContent bool) (*mcp.CallToolResult, error) {
	summary := downloadMediaSummary{DownloadResult: r}
	var extra mcp.Content

	if r.Success && r.Path != "" {
		if info, err := os.Stat(r.Path); err == nil {
			summary.Size = info.Size()
			if chatJID != "" && messageID != "" {
				summary.ResourceURI = mediaResourceURI(chatJID, messageID)
			}
			var data []byte
			if includeContent && info.Size() <= inlineCap(r.MediaType) {
				data, err = os.ReadFile(r.Path)
				if err != nil {
					data = nil
				}
			}
			if data != nil {
				summary.MIMEType = fileMIME(data, r.MediaType, r.Filename)
				extra = inlineContent(data, r.MediaType, summary.MIMEType, summary.ResourceURI)
				summary.Embedded = extra != nil
			} else if summary.ResourceURI != "" {
				extra = mcp.NewResourceLink(summary.ResourceURI, r.Filename,
					"The decrypted file; read it with resources/read.", "")
			}
		}
	}

	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
	}
	contents := []mcp.Content{mcp.NewTextContent(string(b))}
	if extra != nil {
		contents = append(contents, extra)
	}
	return &mcp.CallToolResult{Content: contents}, nil
}

// fileMIME sniffs data's MIME type, and falls back to the filename's
// extension when sniffing finds nothing better than octet-stream (Office
// files, some videos).
func fileMIME(data []byte, mediaType, filename string) string {
	sniffed := sniffResourceMIME(data, mediaType)
	if sniffed == "application/octet-stream" {
		if byExt := mime.TypeByExtension(filepath.Ext(filename)); byExt != "" {
			return byExt
		}
	}
	return sniffed
}

// inlineCap is the largest file of mediaType that downloadMediaResult embeds.
func inlineCap(mediaType string) int64 {
	switch mediaType {
	case "image", "audio":
		return inlineMediaMaxBytes
	default:
		return inlineResourceMaxBytes
	}
}

// inlineContent wraps data in the content block for its media type. The MIME
// type is sniffed by the caller (sniffResourceMIME in resources.go), so it is
// the real format regardless of the filename WhatsApp assigns: all photos are
// named `.jpg` even when the bytes are WebP, and all voice notes `.ogg`.
func inlineContent(data []byte, mediaType, mimeType, uri string) mcp.Content {
	encoded := base64.StdEncoding.EncodeToString(data)
	switch mediaType {
	case "image":
		return mcp.NewImageContent(encoded, mimeType)
	case "audio":
		return mcp.NewAudioContent(encoded, mimeType)
	}
	if uri == "" {
		return nil
	}
	return mcp.NewEmbeddedResource(mcp.BlobResourceContents{
		URI:      uri,
		MIMEType: mimeType,
		Blob:     encoded,
	})
}
