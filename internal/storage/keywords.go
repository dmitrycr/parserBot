package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

type KeywordStore struct {
	path string
}

func NewKeywordStore(path string) KeywordStore {
	return KeywordStore{path: path}
}

func (s KeywordStore) Load(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}

	var keywords []string
	if err := json.Unmarshal(data, &keywords); err != nil {
		return nil, err
	}

	return normalizeKeywordList(keywords), nil
}

func (s KeywordStore) Save(ctx context.Context, keywords []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureDir(s.path); err != nil {
		return err
	}

	data, err := json.MarshalIndent(normalizeKeywordList(keywords), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return os.WriteFile(s.path, data, 0600)
}

func (s KeywordStore) Add(ctx context.Context, keyword string) (bool, error) {
	keywords, err := s.Load(ctx)
	if err != nil {
		return false, err
	}

	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return false, nil
	}

	for _, existing := range keywords {
		if strings.EqualFold(existing, keyword) {
			return false, nil
		}
	}

	keywords = append(keywords, keyword)
	return true, s.Save(ctx, keywords)
}

func (s KeywordStore) Remove(ctx context.Context, keyword string) (bool, error) {
	keywords, err := s.Load(ctx)
	if err != nil {
		return false, err
	}

	keyword = strings.TrimSpace(keyword)
	filtered := keywords[:0]
	var removed bool
	for _, existing := range keywords {
		if strings.EqualFold(existing, keyword) {
			removed = true
			continue
		}
		filtered = append(filtered, existing)
	}

	if !removed {
		return false, nil
	}

	return true, s.Save(ctx, filtered)
}

func normalizeKeywordList(keywords []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(keywords))
	for _, keyword := range keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}

		key := strings.ToLower(keyword)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, keyword)
	}

	return result
}
