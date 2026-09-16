package server

import (
	"regexp"
	"strconv"
	"strings"
)

var explicitVolume = regexp.MustCompile(`(?i)^(.+?)\s*[,\-–—:]?\s+vol(?:ume)?\.?\s*([0-9]+(?:\.[0-9]+)?)(.*)$`)
var volumeSuffix = regexp.MustCompile(`^(?:\s*[:(\[]|\s+[^0-9\s–—-])`)
var volumeMarker = regexp.MustCompile(`(?i)\b(?:vol(?:ume)?\.?|chapters?)\b`)

// Only explicit volume labels are evidence; ordinary numbers and chapter ranges
// must not turn a web novel into a volume. Publisher metadata always wins.
func inferMetadataVolume(metadata bookMetadata, filename string) bookMetadata {
	for _, value := range []string{metadata.Title, filename} {
		value = strings.TrimSpace(strings.ReplaceAll(value, "_", " "))
		if strings.HasSuffix(strings.ToLower(value), ".epub") {
			value = value[:len(value)-5]
		}
		match := explicitVolume.FindStringSubmatch(strings.TrimSpace(value))
		if match == nil || volumeMarker.MatchString(match[1]) || volumeMarker.MatchString(match[3]) || match[3] != "" && !volumeSuffix.MatchString(match[3]) {
			continue
		}
		series := strings.Trim(match[1], " \t\r\n,-–—:")
		volume, err := strconv.ParseFloat(match[2], 64)
		if err != nil || series == "" || volume <= 0 || volume >= 1e6 {
			continue
		}
		if metadata.Series == "" {
			metadata.Series = series
		}
		if metadata.Volume == nil {
			metadata.Volume = &volume
		}
		break
	}
	return metadata
}
