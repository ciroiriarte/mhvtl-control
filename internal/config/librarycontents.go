package config

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseLibraryContents parses a library_contents.<id> stream.
func ParseLibraryContents(r io.Reader) (drives []int, pickers []int, maps []MAPSlot, slots []Slot, err error) {
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "VERSION:") {
			continue
		}
		head, rest, ok := strings.Cut(line, ":")
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("library_contents line %d: missing ':' in %q", lineNo, line)
		}
		f := strings.Fields(head)
		if len(f) != 2 {
			return nil, nil, nil, nil, fmt.Errorf("library_contents line %d: malformed %q", lineNo, line)
		}
		n, e := strconv.Atoi(f[1])
		if e != nil {
			return nil, nil, nil, nil, fmt.Errorf("library_contents line %d: bad number %q", lineNo, f[1])
		}
		barcode := strings.TrimSpace(rest)
		switch f[0] {
		case "Drive":
			drives = append(drives, n)
		case "Picker":
			pickers = append(pickers, n)
		case "MAP":
			maps = append(maps, MAPSlot{Number: n, Barcode: barcode})
		case "Slot":
			slots = append(slots, Slot{Number: n, Barcode: barcode})
		default:
			return nil, nil, nil, nil, fmt.Errorf("library_contents line %d: unknown entry %q", lineNo, f[0])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	return drives, pickers, maps, slots, nil
}
