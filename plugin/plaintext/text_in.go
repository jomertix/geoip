package plaintext

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/v2fly/geoip/lib"
)

const (
	typeTextIn = "text"
	descTextIn = "Convert plaintext IP and CIDR to other formats"
)

func init() {
	lib.RegisterInputConfigCreator(typeTextIn, func(action lib.Action, data json.RawMessage) (lib.InputConverter, error) {
		return newTextIn(action, data)
	})
	lib.RegisterInputConverter(typeTextIn, &textIn{
		Description: descTextIn,
	})
}

func newTextIn(action lib.Action, data json.RawMessage) (lib.InputConverter, error) {
	var tmp struct {
		Name       string     `json:"name"`
		URI        string     `json:"uri"`
		IPOrCIDR   []string   `json:"ipOrCIDR"`
		InputDir   string     `json:"inputDir"`
		Want       []string   `json:"wantedList"`
		OnlyIPType lib.IPType `json:"onlyIPType"`

		RemovePrefixesInLine []string `json:"removePrefixesInLine"`
		RemoveSuffixesInLine []string `json:"removeSuffixesInLine"`
	}

	if len(data) > 0 {
		if err := json.Unmarshal(data, &tmp); err != nil {
			return nil, err
		}
	}

	if tmp.InputDir == "" {
		if tmp.Name == "" {
			return nil, fmt.Errorf("❌ [type %s | action %s] missing inputDir or name", typeTextIn, action)
		}
		if tmp.URI == "" && len(tmp.IPOrCIDR) == 0 {
			return nil, fmt.Errorf("❌ [type %s | action %s] missing uri or ipOrCIDR", typeTextIn, action)
		}
	} else if tmp.Name != "" || tmp.URI != "" || len(tmp.IPOrCIDR) > 0 {
		return nil, fmt.Errorf("❌ [type %s | action %s] inputDir is not allowed to be used with name or uri or ipOrCIDR", typeTextIn, action)
	}

	// Filter want list
	wantList := make(map[string]bool)
	for _, want := range tmp.Want {
		if want = strings.ToUpper(strings.TrimSpace(want)); want != "" {
			wantList[want] = true
		}
	}

	return &textIn{
		Type:        typeTextIn,
		Action:      action,
		Description: descTextIn,
		Name:        tmp.Name,
		URI:         tmp.URI,
		IPOrCIDR:    tmp.IPOrCIDR,
		InputDir:    tmp.InputDir,
		Want:        wantList,
		OnlyIPType:  tmp.OnlyIPType,

		RemovePrefixesInLine: tmp.RemovePrefixesInLine,
		RemoveSuffixesInLine: tmp.RemoveSuffixesInLine,
	}, nil
}

type textIn struct {
	Type        string
	Action      lib.Action
	Description string
	Name        string
	URI         string
	IPOrCIDR    []string
	InputDir    string
	Want        map[string]bool
	OnlyIPType  lib.IPType

	RemovePrefixesInLine []string
	RemoveSuffixesInLine []string
}

func (t *textIn) GetType() string {
	return t.Type
}

func (t *textIn) GetAction() lib.Action {
	return t.Action
}

func (t *textIn) GetDescription() string {
	return t.Description
}

func (t *textIn) Input(container lib.Container) (lib.Container, error) {
	entries := make(map[string]*lib.Entry)
	var err error

	switch {
	case t.InputDir != "":
		err = t.walkDir(t.InputDir, entries)

	case t.Name != "" && t.URI != "":
		switch {
		case strings.HasPrefix(strings.ToLower(t.URI), "http://"), strings.HasPrefix(strings.ToLower(t.URI), "https://"):
			err = t.walkRemoteFile(t.URI, t.Name, entries)
		default:
			err = t.walkLocalFile(t.URI, t.Name, entries)
		}
		if err != nil {
			return nil, err
		}

		fallthrough

	case t.Name != "" && len(t.IPOrCIDR) > 0:
		err = t.appendIPOrCIDR(t.IPOrCIDR, t.Name, entries)

	default:
		return nil, fmt.Errorf("❌ [type %s | action %s] config missing argument inputDir or name or uri or ipOrCIDR", t.Type, t.Action)
	}

	if err != nil {
		return nil, err
	}

	var ignoreIPType lib.IgnoreIPOption
	switch t.OnlyIPType {
	case lib.IPv4:
		ignoreIPType = lib.IgnoreIPv6
	case lib.IPv6:
		ignoreIPType = lib.IgnoreIPv4
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("❌ [type %s | action %s] no entry is generated", t.Type, t.Action)
	}

	for _, entry := range entries {
		switch t.Action {
		case lib.ActionAdd:
			if err := container.Add(entry, ignoreIPType); err != nil {
				return nil, err
			}
		case lib.ActionRemove:
			if err := container.Remove(entry, lib.CaseRemovePrefix, ignoreIPType); err != nil {
				return nil, err
			}
		default:
			return nil, lib.ErrUnknownAction
		}
	}

	return container, nil
}

func (t *textIn) walkDir(dir string, entries map[string]*lib.Entry) error {
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		if err := t.walkLocalFile(path, "", entries); err != nil {
			return err
		}

		return nil
	})

	return err
}

func (t *textIn) walkLocalFile(path, name string, entries map[string]*lib.Entry) error {
	entryName := ""
	name = strings.TrimSpace(name)
	if name != "" {
		entryName = name
	} else {
		entryName = filepath.Base(path)

		// check filename
		if !regexp.MustCompile(`^[a-zA-Z0-9_.\-]+$`).MatchString(entryName) {
			return fmt.Errorf("filename %s cannot be entry name, please remove special characters in it", entryName)
		}

		// remove file extension but not hidden files of which filename starts with "."
		dotIndex := strings.LastIndex(entryName, ".")
		if dotIndex > 0 {
			entryName = entryName[:dotIndex]
		}
	}

	entryName = strings.ToUpper(entryName)

	if len(t.Want) > 0 && !t.Want[entryName] {
		return nil
	}
	if _, found := entries[entryName]; found {
		return fmt.Errorf("found duplicated list %s", entryName)
	}

	entry := lib.NewEntry(entryName)
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := t.scanFile(file, entry); err != nil {
		return err
	}

	entries[entryName] = entry

	return nil
}

func (t *textIn) walkRemoteFile(url, name string, entries map[string]*lib.Entry) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to get remote file %s, http status code %d", url, resp.StatusCode)
	}

	name = strings.ToUpper(name)

	if len(t.Want) > 0 && !t.Want[name] {
		return nil
	}

	entry := lib.NewEntry(name)
	if err := t.scanFile(resp.Body, entry); err != nil {
		return err
	}

	entries[name] = entry

	return nil
}

func (t *textIn) scanFile(reader io.Reader, entry *lib.Entry) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()

		line, _, _ = strings.Cut(line, "#")
		line, _, _ = strings.Cut(line, "//")
		line, _, _ = strings.Cut(line, "/*")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		line = strings.ToLower(line)
		for _, prefix := range t.RemovePrefixesInLine {
			line = strings.TrimSpace(strings.TrimPrefix(line, strings.ToLower(strings.TrimSpace(prefix))))
		}
		for _, suffix := range t.RemoveSuffixesInLine {
			line = strings.TrimSpace(strings.TrimSuffix(line, strings.ToLower(strings.TrimSpace(suffix))))
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if err := entry.AddPrefix(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func (t *textIn) appendIPOrCIDR(ipOrCIDR []string, name string, entries map[string]*lib.Entry) error {
	name = strings.ToUpper(name)

	entry, found := entries[name]
	if !found {
		entry = lib.NewEntry(name)
	}

	for _, cidr := range ipOrCIDR {
		if err := entry.AddPrefix(strings.TrimSpace(cidr)); err != nil {
			return err
		}
	}

	entries[name] = entry

	return nil
}
