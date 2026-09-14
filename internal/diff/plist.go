package diff

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"howett.net/plist"
)

func IsPlist(b []byte) bool {
	if bytes.HasPrefix(b, []byte("bplist00")) {
		return true
	}
	head := b
	if len(head) > 512 {
		head = head[:512]
	}
	return bytes.Contains(head, []byte("<plist")) || bytes.Contains(head, []byte("<!DOCTYPE plist"))
}

// DecodePlist parses binary, XML or OpenStep plists.
func DecodePlist(b []byte) (any, error) {
	var v any
	_, err := plist.Unmarshal(b, &v)
	return v, err
}

// PlistText renders a plist as stable, sorted, indented key = value text
// so binary and XML plists diff line by line the same way.
func PlistText(b []byte) (string, error) {
	v, err := DecodePlist(b)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	writePlist(&sb, v, 0)
	return sb.String(), nil
}

func writePlist(sb *strings.Builder, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch t[k].(type) {
			case map[string]any, []any:
				fmt.Fprintf(sb, "%s%s:\n", pad, k)
				writePlist(sb, t[k], indent+1)
			default:
				fmt.Fprintf(sb, "%s%s = %s\n", pad, k, scalar(t[k]))
			}
		}
	case []any:
		for _, e := range t {
			switch e.(type) {
			case map[string]any, []any:
				fmt.Fprintf(sb, "%s-\n", pad)
				writePlist(sb, e, indent+1)
			default:
				fmt.Fprintf(sb, "%s- %s\n", pad, scalar(e))
			}
		}
	default:
		fmt.Fprintf(sb, "%s%s\n", pad, scalar(v))
	}
}

func scalar(v any) string {
	switch t := v.(type) {
	case []byte:
		if len(t) > 32 {
			return fmt.Sprintf("<%d bytes>", len(t))
		}
		return fmt.Sprintf("<%x>", t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case string:
		return fmt.Sprintf("%q", t)
	}
	return fmt.Sprint(v)
}
