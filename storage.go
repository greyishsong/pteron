package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var titlePattern = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
var headingPattern = regexp.MustCompile(`(?is)<h1\b[^>]*>(.*?)</h1\s*>`)
var tagsPattern = regexp.MustCompile(`(?s)<[^>]*>`)
var errConflict = errors.New("site already exists")
var errInvalidID = errors.New("invalid site ID: use 1–64 letters, digits, underscores or hyphens")

type Metadata struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Feedback struct {
	SiteID      string         `json:"site_id"`
	UpdatedAt   string         `json:"updated_at"`
	Submitted   bool           `json:"submitted"`
	SubmittedAt string         `json:"submitted_at"`
	Fields      map[string]any `json:"fields"`
	Extra       map[string]any `json:"extra"`
}

type Site struct {
	Metadata
	URL      string   `json:"url"`
	LocalURL string   `json:"local_url"`
	Feedback Feedback `json:"feedback"`
}

type Store struct {
	root string
	mu   sync.RWMutex
}

func newStore(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "sites")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func now() string { return time.Now().Format(time.RFC3339Nano) }
func emptyFeedback(id string) Feedback {
	return Feedback{SiteID: id, Fields: map[string]any{}, Extra: map[string]any{}}
}
func extractTitle(source string) string {
	for _, pattern := range []*regexp.Regexp{titlePattern, headingPattern} {
		if match := pattern.FindStringSubmatch(source); len(match) > 1 {
			title := strings.Join(strings.Fields(html.UnescapeString(tagsPattern.ReplaceAllString(match[1], ""))), " ")
			if title != "" {
				return title
			}
		}
	}
	return ""
}
func randomID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, 8)
	for i := range result {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		result[i] = alphabet[n.Int64()]
	}
	return string(result), nil
}

// atomicWrite never exposes partial files, including during concurrent autosaves.
func atomicWrite(path string, content []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pteron-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'))
}
func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// Caller must hold mu. Reject symlink directories as well as invalid slugs.
func (s *Store) dir(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", errInvalidID
	}
	dir := filepath.Join(s.root, id)
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fs.ErrNotExist
	}
	return dir, nil
}
func (s *Store) metadata(id string) (Metadata, error) {
	var meta Metadata
	dir, err := s.dir(id)
	if err != nil {
		return meta, err
	}
	err = readJSON(filepath.Join(dir, "meta.json"), &meta)
	return meta, err
}
func (s *Store) feedback(id string) (Feedback, error) {
	feedback := emptyFeedback(id)
	dir, err := s.dir(id)
	if err != nil {
		return feedback, err
	}
	err = readJSON(filepath.Join(dir, "feedback.json"), &feedback)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if feedback.Fields == nil {
		feedback.Fields = map[string]any{}
	}
	if feedback.Extra == nil {
		feedback.Extra = map[string]any{}
	}
	return feedback, err
}
func (s *Store) create(id, title, source string) (Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var meta Metadata
	generated := id == ""
	for {
		if generated {
			var err error
			id, err = randomID()
			if err != nil {
				return meta, err
			}
		}
		if !validID.MatchString(id) {
			return meta, errInvalidID
		}
		err := os.Mkdir(filepath.Join(s.root, id), 0700)
		if errors.Is(err, fs.ErrExist) {
			if generated {
				continue
			}
			return meta, errConflict
		}
		if err != nil {
			return meta, err
		}
		break
	}
	dir := filepath.Join(s.root, id)
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()
	if title == "" {
		title = extractTitle(source)
	}
	if title == "" {
		title = "Untitled"
	}
	timestamp := now()
	meta = Metadata{ID: id, Title: title, CreatedAt: timestamp, UpdatedAt: timestamp}
	if err := atomicWrite(filepath.Join(dir, "index.html"), []byte(source)); err != nil {
		return Metadata{}, err
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
		return Metadata{}, err
	}
	if err := writeJSON(filepath.Join(dir, "feedback.json"), emptyFeedback(id)); err != nil {
		return Metadata{}, err
	}
	success = true
	return meta, nil
}
func (s *Store) update(id, title, source string) (Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := s.metadata(id)
	if err != nil {
		return meta, err
	}
	if title == "" {
		title = extractTitle(source)
	}
	if title != "" {
		meta.Title = title
	}
	meta.UpdatedAt = now()
	dir := filepath.Join(s.root, id)
	if err = atomicWrite(filepath.Join(dir, "index.html"), []byte(source)); err != nil {
		return meta, err
	}
	err = writeJSON(filepath.Join(dir, "meta.json"), meta)
	return meta, err
}
func (s *Store) list(ip string, port int) ([]Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	sites := []Site{}
	for _, entry := range entries {
		if !entry.IsDir() || !validID.MatchString(entry.Name()) {
			continue
		}
		meta, err := s.metadata(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read site %s: %w", entry.Name(), err)
		}
		feedback, err := s.feedback(entry.Name())
		if err != nil {
			return nil, err
		}
		sites = append(sites, siteInfo(meta, feedback, ip, port))
	}
	sort.Slice(sites, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, sites[i].UpdatedAt)
		b, _ := time.Parse(time.RFC3339Nano, sites[j].UpdatedAt)
		if a.Equal(b) {
			return sites[i].ID < sites[j].ID
		}
		return a.After(b)
	})
	return sites, nil
}
func siteInfo(meta Metadata, feedback Feedback, ip string, port int) Site {
	return Site{Metadata: meta, URL: fmt.Sprintf("http://%s:%d/s/%s", ip, port, meta.ID), LocalURL: fmt.Sprintf("http://127.0.0.1:%d/s/%s", port, meta.ID), Feedback: feedback}
}

type FeedbackUpdate struct {
	Fields    map[string]any `json:"fields"`
	Extra     map[string]any `json:"extra"`
	Submitted bool           `json:"submitted"`
}

func (s *Store) saveFeedback(id string, update FeedbackUpdate, clear bool) (Feedback, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	feedback, err := s.feedback(id)
	if err != nil {
		return feedback, err
	}
	if clear {
		feedback = emptyFeedback(id)
	} else {
		// Merge so dynamically rendered or temporarily absent fields are preserved.
		for key, value := range update.Fields {
			feedback.Fields[key] = value
		}
		for key, value := range update.Extra {
			feedback.Extra[key] = value
		}
		if update.Submitted {
			feedback.Submitted = true
			feedback.SubmittedAt = now()
		}
	}
	feedback.UpdatedAt = now()
	err = writeJSON(filepath.Join(s.root, id, "feedback.json"), feedback)
	return feedback, err
}
func (s *Store) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.dir(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
