package boardspec

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var markerRE = regexp.MustCompile(`(?m)\n*<!-- clipse-ref: (\S+) sha:(\S+) -->\s*$`)

// ContentSHA is a short digest over an issue's reconciled content (title,
// body, labels, deps) — NOT its ref. A re-run compares this to the sha stored
// in the on-board marker to decide create/update/skip. Labels and deps are
// sorted so ordering never changes the digest.
func ContentSHA(is Issue) string {
	labels := append([]string(nil), is.Labels...)
	deps := append([]string(nil), is.Deps...)
	sort.Strings(labels)
	sort.Strings(deps)
	h := sha256.New()
	fmt.Fprintf(h, "title:%s\x00body:%s\x00labels:%s\x00deps:%s",
		is.Title, is.Body, strings.Join(labels, ","), strings.Join(deps, ","))
	return fmt.Sprintf("%x", h.Sum(nil))[:8]
}

// RenderMarker formats the hidden trailer stored on a Linear issue.
func RenderMarker(ref, sha string) string {
	return fmt.Sprintf("<!-- clipse-ref: %s sha:%s -->", ref, sha)
}

// WithBody is the full Linear description for an issue: its body followed by
// the marker carrying its ref and current content sha.
func WithBody(is Issue) string {
	return strings.TrimRight(is.Body, "\n") + "\n\n" + RenderMarker(is.Ref, ContentSHA(is))
}

// ParseMarker extracts (ref, sha) from a Linear description's trailer.
func ParseMarker(description string) (string, string, bool) {
	m := markerRE.FindStringSubmatch(description)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// StripMarker returns description without its trailing marker (trimmed).
func StripMarker(description string) string {
	return strings.TrimSpace(markerRE.ReplaceAllString(description, ""))
}
