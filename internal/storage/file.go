package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Chat struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	AccessHash int64  `json:"access_hash"`
	Title      string `json:"title,omitempty"`
	Username   string `json:"username,omitempty"`
	Enabled    bool   `json:"enabled"`
}

type Match struct {
	ChatID       int64     `json:"chat_id"`
	ChatType     string    `json:"chat_type"`
	ChatTitle    string    `json:"chat_title,omitempty"`
	ChatUsername string    `json:"chat_username,omitempty"`
	MessageID    int       `json:"message_id"`
	Keyword      string    `json:"keyword"`
	Text         string    `json:"text"`
	Date         time.Time `json:"date"`
	ParsedAt     time.Time `json:"parsed_at"`
	Views        int       `json:"views"`
}

type FileStore struct {
	chatsPath       string
	checkpointsPath string
	matchesPath     string
}

func NewFileStore(chatsPath string, checkpointsPath string, matchesPath string) FileStore {
	return FileStore{
		chatsPath:       chatsPath,
		checkpointsPath: checkpointsPath,
		matchesPath:     matchesPath,
	}
}

func (s FileStore) LoadChats(ctx context.Context) ([]Chat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.chatsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s not found: create it with sync-chats", s.chatsPath)
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}

	var chats []Chat
	if err := json.Unmarshal(data, &chats); err != nil {
		return nil, err
	}

	return chats, nil
}

func (s FileStore) SaveChats(ctx context.Context, chats []Chat) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureDir(s.chatsPath); err != nil {
		return err
	}

	data, err := json.MarshalIndent(chats, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return os.WriteFile(s.chatsPath, data, 0600)
}

func (s FileStore) LoadCheckpoints(ctx context.Context) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.checkpointsPath)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]int{}, nil
	}

	var checkpoints map[string]int
	if err := json.Unmarshal(data, &checkpoints); err != nil {
		return nil, err
	}
	if checkpoints == nil {
		checkpoints = map[string]int{}
	}

	return checkpoints, nil
}

func (s FileStore) SaveCheckpoints(ctx context.Context, checkpoints map[string]int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureDir(s.checkpointsPath); err != nil {
		return err
	}

	data, err := json.MarshalIndent(checkpoints, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return os.WriteFile(s.checkpointsPath, data, 0600)
}

func (s FileStore) ResetCheckpoint(ctx context.Context, chat Chat) error {
	checkpoints, err := s.LoadCheckpoints(ctx)
	if err != nil {
		return err
	}
	delete(checkpoints, CheckpointKey(chat))
	return s.SaveCheckpoints(ctx, checkpoints)
}

func (s FileStore) AppendMatch(ctx context.Context, match Match) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureDir(s.matchesPath); err != nil {
		return err
	}

	data, err := json.Marshal(match)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	file, err := os.OpenFile(s.matchesPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(data)
	return err
}

func CheckpointKey(chat Chat) string {
	return chat.Type + ":" + strconv.FormatInt(chat.ID, 10)
}

func IsPersonalChat(chat Chat) bool {
	return chat.Type == "user" || chat.Type == "private"
}

func ValidateChats(chats []Chat) error {
	var errs []string
	for i, chat := range chats {
		if err := ValidateChat(chat); err != nil {
			errs = append(errs, fmt.Sprintf("chat[%d] %s: %v", i, ChatLogValue(chat), err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}

	return nil
}

func ValidateChat(chat Chat) error {
	if chat.ID == 0 {
		return errors.New("id is required")
	}

	switch chat.Type {
	case "user", "group", "channel", "supergroup":
	default:
		return fmt.Errorf("unsupported type %q", chat.Type)
	}

	if chat.Type != "group" && chat.AccessHash == 0 {
		return fmt.Errorf("access_hash is required for %s", chat.Type)
	}

	return nil
}

func ChatLogValue(chat Chat) string {
	return fmt.Sprintf(
		"type=%s id=%d access_hash=%d title=%q username=%q enabled=%t",
		chat.Type,
		chat.ID,
		chat.AccessHash,
		chat.Title,
		chat.Username,
		chat.Enabled,
	)
}

func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}

	return os.MkdirAll(dir, 0700)
}
