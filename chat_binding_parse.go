package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

var errBindUsage = errors.New("invalid bind arguments")
var errBindDirectory = errors.New("binding directory does not exist or is not accessible")

const bindUsageMessage = "Usage: /bind <directory> [pairing_code]."
const bindDirectoryMessage = "The directory does not exist or is not accessible."

func isBindCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "/bind")
}

func parsePairedBindArgs(text string) ([]string, error) {
	if len(text) > 8192 || !utf8.ValidString(text) || strings.ContainsAny(text, "\r\n\x00") {
		return nil, errBindUsage
	}
	text = strings.TrimSpace(text)
	end := strings.IndexFunc(text, unicode.IsSpace)
	if end < 0 || !strings.EqualFold(text[:end], "/bind") {
		return nil, errBindUsage
	}
	text = strings.TrimSpace(text[end:])
	var args []string
	for text != "" {
		if len(args) == 2 {
			return nil, errBindUsage
		}
		var value strings.Builder
		if text[0] == '"' {
			text = text[1:]
			closed := false
			for len(text) > 0 {
				ch := text[0]
				text = text[1:]
				if ch == '"' {
					closed = true
					break
				}
				if ch == '\\' && len(text) > 0 && (text[0] == '"' || text[0] == '\\') {
					ch = text[0]
					text = text[1:]
				}
				value.WriteByte(ch)
			}
			if !closed {
				return nil, errBindUsage
			}
			if text != "" {
				r, _ := utf8.DecodeRuneInString(text)
				if !unicode.IsSpace(r) {
					return nil, errBindUsage
				}
			}
		} else {
			end = strings.IndexFunc(text, unicode.IsSpace)
			if end < 0 {
				end = len(text)
			}
			value.WriteString(text[:end])
			text = text[end:]
		}
		if value.Len() == 0 {
			return nil, errBindUsage
		}
		args = append(args, value.String())
		text = strings.TrimSpace(text)
	}
	if len(args) == 0 {
		return nil, errBindUsage
	}
	return args, nil
}

func canonicalBindingDirectory(path string) (string, os.FileInfo, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, errBindDirectory
		}
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		return "", nil, errBindDirectory
	}
	path, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", nil, errBindDirectory
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", nil, errBindDirectory
	}
	f, err := os.Open(path)
	if err != nil {
		return "", nil, errBindDirectory
	}
	defer f.Close()
	if _, err = f.Readdirnames(1); err != nil && err != io.EOF {
		return "", nil, errBindDirectory
	}
	return path, info, nil
}
