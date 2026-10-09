package main

import (
	"os/exec"
	"regexp"
)

var urlPattern = regexp.MustCompile(`https?://[^\s"'` + "`" + `<>\[\]{}]+`)

// urlAtCol returns the URL substring in line that contains rune column col,
// or "" if none.
func urlAtCol(line []rune, col int) string {
	s := string(line)
	for _, loc := range urlPattern.FindAllStringIndex(s, -1) {
		// loc indices are byte offsets into s; since we only match ASCII-safe
		// URL characters, rune and byte offsets coincide here.
		if col >= loc[0] && col < loc[1] {
			return s[loc[0]:loc[1]]
		}
	}
	return ""
}

func openURL(url string) {
	for _, bin := range []string{"xdg-open", "open"} {
		if _, err := exec.LookPath(bin); err == nil {
			exec.Command(bin, url).Start()
			return
		}
	}
}
