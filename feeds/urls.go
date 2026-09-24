package feeds

import (
	"mime"
	"net/url"
	"path"
	"strings"

	"aunefyren/solstein/signing"

	"github.com/google/uuid"
)

// URLs builds the feed and episode URLs Solstein writes out.
type URLs struct {
	// Base is the scheme and host clients reach Solstein at, without a
	// trailing slash.
	Base   string
	Signer signing.Signer
	// Unsigned leaves signatures off, when auth is disabled.
	Unsigned bool
}

// FeedPath is the path of a feed's signed URL.
func FeedPath(feedID uuid.UUID) string {
	return "/api/feeds/" + feedID.String() + ".xml"
}

// EpisodePath is the path of an episode's signed URL. It ends in the audio's
// extension, because ABS names the downloaded file after it.
func EpisodePath(feedID, episodeID uuid.UUID, extension string) string {
	return "/api/episodes/" + feedID.String() + "/" + episodeID.String() + "." + extension
}

// Feed returns a feed's full, signed URL.
func (urls URLs) Feed(feedID uuid.UUID) string {
	return urls.signed(FeedPath(feedID))
}

// Episode returns an episode's full, signed URL.
func (urls URLs) Episode(feedID, episodeID uuid.UUID, extension string) string {
	return urls.signed(EpisodePath(feedID, episodeID, extension))
}

func (urls URLs) signed(path string) string {
	if urls.Unsigned {
		return urls.Base + path
	}
	return urls.Base + path + "?" + signing.QueryParameter + "=" + urls.Signer.Sign(path)
}

// audioExtensions are those ABS accepts as audio (globals.SupportedAudioTypes
// covers these and more).
var audioExtensions = map[string]bool{
	"mp3": true, "m4a": true, "m4b": true, "aac": true, "ogg": true, "oga": true,
	"opus": true, "flac": true, "wav": true, "wma": true, "mp4": true, "webm": true,
}

var extensionsByType = map[string]string{
	"audio/mpeg":  "mp3",
	"audio/mp3":   "mp3",
	"audio/mp4":   "m4a",
	"audio/x-m4a": "m4a",
	"audio/aac":   "aac",
	"audio/ogg":   "ogg",
	"audio/opus":  "opus",
	"audio/flac":  "flac",
	"audio/wav":   "wav",
	"audio/x-wav": "wav",
	"video/mp4":   "mp4",
}

// AudioExtension picks the file extension for an episode URL: the source
// URL's own extension when it is an audio one, else one matching the MIME
// type, else mp3 (as ABS assumes).
func AudioExtension(sourceURL, mimeType string) string {
	if parsed, err := url.Parse(sourceURL); err == nil {
		extension := strings.ToLower(strings.TrimPrefix(path.Ext(parsed.Path), "."))
		if audioExtensions[extension] {
			return extension
		}
	}
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		if extension, ok := extensionsByType[strings.ToLower(mediaType)]; ok {
			return extension
		}
	}
	return "mp3"
}
