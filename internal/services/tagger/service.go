package tagger

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	id3v2 "github.com/bogem/id3v2/v2"
	"github.com/go-flac/flacvorbis"
	flac "github.com/go-flac/go-flac"
)

type TrackMeta struct {
	Title       string
	Artist      string
	Album       string
	TrackNumber int
	DiscNumber  int
	Year        int
	CoverURL    string
	// MusicBrainz identities — written only when set (provider == musicbrainz).
	// RecordingID is release-independent; the rest describe the specific
	// release the provider tracklist came from.
	MBRecordingID    string
	MBTrackID        string // release-track id
	MBReleaseGroupID string
	MBArtistID       string
	MBAlbumArtistID  string
}

func Tag(filePath string, meta TrackMeta) error {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".mp3":
		return tagMP3(filePath, meta)
	case ".flac":
		return tagFLAC(filePath, meta)
	case ".wav":
		return tagWAV(filePath, meta)
	default:
		slog.Warn("tagger: unsupported format", "ext", ext)
		return nil
	}
}

func tagMP3(filePath string, meta TrackMeta) error {
	// Parse existing frames so foreign tags (UFID, TSRC, TXXX, ReplayGain, …)
	// survive; we only replace Crate's own frames below. Opening with
	// Parse:false would drop every frame not re-written here.
	tag, err := id3v2.Open(filePath, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("tagger: open mp3: %w", err)
	}
	defer tag.Close()

	tag.SetDefaultEncoding(id3v2.EncodingUTF8)
	tag.SetTitle(meta.Title)
	tag.SetArtist(meta.Artist)
	tag.SetAlbum(meta.Album)
	tag.SetYear(fmt.Sprintf("%d", meta.Year))

	// SetTitle/SetArtist/etc. replace, but AddTextFrame/AddAttachedPicture
	// append — so clear Crate's track/disc/cover frames first, or a re-tag of
	// an already-tagged file (Soulseek files arrive tagged) accumulates dupes.
	trackID := tag.CommonID("Track number/Position in set")
	discID := tag.CommonID("Part of a set")
	tag.DeleteFrames(trackID)
	tag.DeleteFrames(discID)
	if meta.TrackNumber > 0 {
		tag.AddTextFrame(trackID, id3v2.EncodingUTF8, fmt.Sprintf("%d", meta.TrackNumber))
	}
	if meta.DiscNumber > 0 {
		tag.AddTextFrame(discID, id3v2.EncodingUTF8, fmt.Sprintf("%d", meta.DiscNumber))
	}

	tagMB3(tag, meta)

	if meta.CoverURL != "" {
		if pic := fetchCover(meta.CoverURL); pic != nil {
			tag.DeleteFrames(tag.CommonID("Attached picture"))
			tag.AddAttachedPicture(id3v2.PictureFrame{
				Encoding:    id3v2.EncodingUTF8,
				MimeType:    pic.mimeType,
				PictureType: id3v2.PTFrontCover,
				Picture:     pic.data,
			})
		}
	}

	return tag.Save()
}

// crateOwnedFLACFields are the Vorbis comments Crate manages. On (re-)tag we
// overwrite only these and preserve every other field — ReplayGain,
// MusicBrainz, AcoustID, ISRC, etc. written by other tools such as Music
// Assistant's analysis providers. Rebuilding a fresh comment block (the old
// behavior) silently wiped those.
var crateOwnedFLACFields = map[string]bool{
	"TITLE":       true,
	"ARTIST":      true,
	"ALBUM":       true,
	"TRACKNUMBER": true,
	"DISCNUMBER":  true,
	"DATE":        true,
}

func tagFLAC(filePath string, meta TrackMeta) error {
	f, err := flac.ParseFile(filePath)
	if err != nil {
		return fmt.Errorf("tagger: open flac: %w", err)
	}

	// Start from the existing comments so foreign fields survive; only strip
	// the fields Crate is about to rewrite. Falls back to a fresh block when
	// the file has no (or an unparseable) Vorbis comment block.
	// MB fields we have values for are stripped-and-rewritten; foreign MB
	// fields we don't manage (ALBUMID, ARTISTTYPE, …) stay untouched.
	mbFields := flacMBFields(meta)
	cmtIdx := -1
	cmt := flacvorbis.New()
	for i, block := range f.Meta {
		if block.Type != flac.VorbisComment {
			continue
		}
		cmtIdx = i
		if existing, perr := flacvorbis.ParseFromMetaDataBlock(*block); perr == nil {
			cmt = existing
			kept := cmt.Comments[:0]
			for _, c := range cmt.Comments {
				key, _, ok := strings.Cut(c, "=")
				if !ok {
					continue
				}
				k := strings.ToUpper(strings.TrimSpace(key))
				if crateOwnedFLACFields[k] || mbFields[k] != "" {
					continue
				}
				kept = append(kept, c)
			}
			cmt.Comments = kept
		}
		break
	}

	cmt.Add(flacvorbis.FIELD_TITLE, meta.Title)
	cmt.Add(flacvorbis.FIELD_ARTIST, meta.Artist)
	cmt.Add(flacvorbis.FIELD_ALBUM, meta.Album)
	cmt.Add(flacvorbis.FIELD_TRACKNUMBER, fmt.Sprintf("%d", meta.TrackNumber))
	cmt.Add("DISCNUMBER", fmt.Sprintf("%d", meta.DiscNumber))
	if meta.Year > 0 {
		cmt.Add("DATE", fmt.Sprintf("%d", meta.Year))
	}
	for k, v := range mbFields {
		cmt.Add(k, v)
	}

	cmtBlock := cmt.Marshal()
	if cmtIdx >= 0 {
		f.Meta[cmtIdx] = &cmtBlock
	} else {
		f.Meta = append(f.Meta, &cmtBlock)
	}

	if meta.CoverURL != "" {
		if pic := fetchCover(meta.CoverURL); pic != nil {
			filtered := f.Meta[:0]
			for _, block := range f.Meta {
				if block.Type != flac.Picture {
					filtered = append(filtered, block)
				}
			}
			f.Meta = append(filtered, buildFLACPicture(pic))
		}
	}

	return f.Save(filePath)
}

// flacMBFields maps Vorbis field names to the MusicBrainz ids we have values
// for — empty set for non-MB tracks, so nothing is stripped or written.
func flacMBFields(meta TrackMeta) map[string]string {
	m := map[string]string{}
	if meta.MBRecordingID != "" {
		m["MUSICBRAINZ_TRACKID"] = meta.MBRecordingID
	}
	if meta.MBTrackID != "" {
		m["MUSICBRAINZ_RELEASETRACKID"] = meta.MBTrackID
	}
	if meta.MBReleaseGroupID != "" {
		m["MUSICBRAINZ_RELEASEGROUPID"] = meta.MBReleaseGroupID
	}
	if meta.MBArtistID != "" {
		m["MUSICBRAINZ_ARTISTID"] = meta.MBArtistID
	}
	if meta.MBAlbumArtistID != "" {
		m["MUSICBRAINZ_ALBUMARTISTID"] = meta.MBAlbumArtistID
	}
	return m
}

// tagMB3 writes MusicBrainz ids into an ID3v2 tag — recording id as UFID
// (owner http://musicbrainz.org, the Picard/AcoustID convention) and the rest
// as TXXX frames with Picard's descriptions. Foreign TXXX/UFID frames (e.g.
// ReplayGain) are preserved: the whole frame class is swapped out, foreign
// frames re-added, then ours appended.
func tagMB3(tag *id3v2.Tag, meta TrackMeta) {
	txxx := map[string]string{}
	if meta.MBTrackID != "" {
		txxx["MusicBrainz Release Track Id"] = meta.MBTrackID
	}
	if meta.MBReleaseGroupID != "" {
		txxx["MusicBrainz Release Group Id"] = meta.MBReleaseGroupID
	}
	if meta.MBArtistID != "" {
		txxx["MusicBrainz Artist Id"] = meta.MBArtistID
	}
	if meta.MBAlbumArtistID != "" {
		txxx["MusicBrainz Album Artist Id"] = meta.MBAlbumArtistID
	}
	if len(txxx) == 0 && meta.MBRecordingID == "" {
		return
	}

	if len(txxx) > 0 {
		var foreign []id3v2.Framer
		for _, f := range tag.GetFrames("TXXX") {
			udf, ok := f.(id3v2.UserDefinedTextFrame)
			if !ok {
				foreign = append(foreign, f)
				continue
			}
			if _, managed := txxx[udf.Description]; !managed {
				foreign = append(foreign, f)
			}
		}
		tag.DeleteFrames("TXXX")
		for _, f := range foreign {
			tag.AddFrame("TXXX", f)
		}
		for desc, val := range txxx {
			tag.AddUserDefinedTextFrame(id3v2.UserDefinedTextFrame{
				Encoding:    id3v2.EncodingUTF8,
				Description: desc,
				Value:       val,
			})
		}
	}

	if meta.MBRecordingID != "" {
		var foreign []id3v2.Framer
		for _, f := range tag.GetFrames("UFID") {
			if ufid, ok := f.(id3v2.UFIDFrame); !ok || ufid.OwnerIdentifier != "http://musicbrainz.org" {
				foreign = append(foreign, f)
			}
		}
		tag.DeleteFrames("UFID")
		for _, f := range foreign {
			tag.AddFrame("UFID", f)
		}
		tag.AddUFIDFrame(id3v2.UFIDFrame{
			OwnerIdentifier: "http://musicbrainz.org",
			Identifier:      []byte(meta.MBRecordingID),
		})
	}
}

func tagWAV(filePath string, meta TrackMeta) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("tagger: read wav: %w", err)
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return fmt.Errorf("tagger: not a valid WAV file")
	}

	// Strip existing LIST INFO chunk if present
	cleaned := stripListInfo(data)

	// Build new LIST INFO chunk
	fields := map[string]string{
		"INAM": meta.Title,
		"IART": meta.Artist,
		"IPRD": meta.Album,
		"ITRK": fmt.Sprintf("%d", meta.TrackNumber),
		"IKEY": fmt.Sprintf("%d", meta.DiscNumber),
		"ICRD": fmt.Sprintf("%d", meta.Year),
	}
	info := buildListInfo(fields)

	// Append LIST chunk after existing sub-chunks
	result := make([]byte, 0, len(cleaned)+len(info))
	result = append(result, cleaned[:12]...)
	result = append(result, cleaned[12:]...)
	result = append(result, info...)

	// Update RIFF size
	riffSize := uint32(len(result) - 8)
	result[4] = byte(riffSize)
	result[5] = byte(riffSize >> 8)
	result[6] = byte(riffSize >> 16)
	result[7] = byte(riffSize >> 24)

	return os.WriteFile(filePath, result, 0600) // #nosec G703 -- filePath is from internal organizer, not user input
}

func stripListInfo(data []byte) []byte {
	pos := 12
	var result []byte
	result = append(result, data[:12]...)
	for pos+8 <= len(data) {
		chunkID := string(data[pos : pos+4])
		chunkSize := int(data[pos+4]) | int(data[pos+5])<<8 | int(data[pos+6])<<16 | int(data[pos+7])<<24
		totalSize := 8 + chunkSize
		if chunkSize%2 != 0 {
			totalSize++
		}
		if pos+totalSize > len(data) {
			totalSize = len(data) - pos
		}
		if chunkID == "LIST" && pos+12 <= len(data) && string(data[pos+8:pos+12]) == "INFO" {
			pos += totalSize
			continue
		}
		result = append(result, data[pos:pos+totalSize]...)
		pos += totalSize
	}
	return result
}

func buildListInfo(fields map[string]string) []byte {
	keys := []string{"INAM", "IART", "IPRD", "ITRK", "IKEY", "ICRD", "ICMT"}
	var payload []byte
	payload = append(payload, []byte("INFO")...)
	for _, key := range keys {
		val := fields[key]
		if val == "" || val == "0" {
			continue
		}
		valBytes := append([]byte(val), 0)
		size := len(valBytes)
		payload = append(payload, []byte(key)...)
		payload = append(payload, byte(size), byte(size>>8), byte(size>>16), byte(size>>24))
		payload = append(payload, valBytes...)
		if size%2 != 0 {
			payload = append(payload, 0)
		}
	}
	// LIST header: "LIST" + size (4 bytes LE)
	listSize := len(payload)
	header := []byte("LIST")
	header = append(header, byte(listSize), byte(listSize>>8), byte(listSize>>16), byte(listSize>>24))
	return append(header, payload...)
}

type coverData struct {
	data     []byte
	mimeType string
}

func fetchCover(url string) *coverData {
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		slog.Warn("tagger: fetch cover failed", "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	data, err := readAll(resp.Body, 5<<20)
	if err != nil {
		return nil
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}
	return &coverData{data: data, mimeType: mime}
}

func readAll(r interface{ Read([]byte) (int, error) }, max int) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > max {
				return nil, fmt.Errorf("too large")
			}
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func buildFLACPicture(pic *coverData) *flac.MetaDataBlock {
	// FLAC picture block: type(4) + mime_len(4) + mime + desc_len(4) + desc +
	// width(4) + height(4) + depth(4) + colors(4) + data_len(4) + data
	mime := []byte(pic.mimeType)
	data := pic.data

	size := 4 + 4 + len(mime) + 4 + 0 + 4 + 4 + 4 + 4 + 4 + len(data)
	buf := make([]byte, size)
	off := 0

	putU32 := func(v uint32) {
		buf[off] = byte(v >> 24)
		buf[off+1] = byte(v >> 16)
		buf[off+2] = byte(v >> 8)
		buf[off+3] = byte(v)
		off += 4
	}

	putU32(3) // Front cover
	putU32(uint32(len(mime)))
	copy(buf[off:], mime)
	off += len(mime)
	putU32(0) // description length
	putU32(0) // width
	putU32(0) // height
	putU32(0) // color depth
	putU32(0) // colors used
	putU32(uint32(len(data)))
	copy(buf[off:], data)

	block := &flac.MetaDataBlock{
		Type: flac.Picture,
		Data: buf,
	}
	return block
}
