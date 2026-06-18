package app

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	msgparser "parserTgChat/internal/parser"
	"parserTgChat/internal/storage"
	"parserTgChat/internal/telegram"
)

type Config struct {
	ChatsPath       string
	CheckpointsPath string
	MatchesPath     string
	KeywordsPath    string
}

type Service struct {
	tg       *telegram.Client
	files    storage.FileStore
	keywords storage.KeywordStore
}

type ParseOptions struct {
	Limit int
}

type ParseSummary struct {
	Enabled int
	Failed  int
	Matches int
}

func NewService(tg *telegram.Client, config Config) Service {
	return Service{
		tg:       tg,
		files:    storage.NewFileStore(config.ChatsPath, config.CheckpointsPath, config.MatchesPath),
		keywords: storage.NewKeywordStore(config.KeywordsPath),
	}
}

func (s Service) SyncChats(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	dialogs, err := s.tg.Dialogs(ctx, limit)
	if err != nil {
		return 0, err
	}

	return s.saveDialogs(ctx, dialogs)
}

func (s Service) SyncChatsInFolder(ctx context.Context, folderID int, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	dialogs, err := s.tg.DialogsInFolder(ctx, folderID, limit)
	if err != nil {
		return 0, err
	}

	return s.saveDialogs(ctx, dialogs)
}

func (s Service) Folders(ctx context.Context) ([]telegram.DialogFolder, error) {
	return s.tg.DialogFolders(ctx)
}

func (s Service) saveDialogs(ctx context.Context, dialogs []telegram.Dialog) (int, error) {
	existing, _ := s.files.LoadChats(ctx)
	enabled := map[string]bool{}
	for _, chat := range existing {
		enabled[storage.CheckpointKey(chat)] = chat.Enabled
	}

	chats := make([]storage.Chat, 0, len(dialogs))
	for _, dialog := range dialogs {
		chat := storage.Chat{
			ID:         dialog.ID,
			Type:       dialog.Type,
			AccessHash: dialog.AccessHash,
			Title:      dialog.Title,
			Username:   dialog.Username,
			Enabled:    false,
		}
		if value, ok := enabled[storage.CheckpointKey(chat)]; ok {
			chat.Enabled = value
		}
		chats = append(chats, chat)
	}

	if err := s.files.SaveChats(ctx, chats); err != nil {
		return 0, err
	}

	return len(chats), nil
}

func (s Service) Chats(ctx context.Context) ([]storage.Chat, error) {
	return s.files.LoadChats(ctx)
}

func (s Service) SetChatEnabled(ctx context.Context, chatType string, id int64, enabled bool) (storage.Chat, error) {
	chats, err := s.files.LoadChats(ctx)
	if err != nil {
		return storage.Chat{}, err
	}

	for i, chat := range chats {
		if chat.Type == chatType && chat.ID == id {
			chats[i].Enabled = enabled
			if err := s.files.SaveChats(ctx, chats); err != nil {
				return storage.Chat{}, err
			}
			return chats[i], nil
		}
	}

	return storage.Chat{}, fmt.Errorf("chat not found type=%s id=%d", chatType, id)
}

func (s Service) ResetCheckpoint(ctx context.Context, chatType string, id int64) (storage.Chat, error) {
	chat, err := s.FindChat(ctx, chatType, id)
	if err != nil {
		return storage.Chat{}, err
	}
	if err := s.files.ResetCheckpoint(ctx, chat); err != nil {
		return storage.Chat{}, err
	}

	return chat, nil
}

func (s Service) ResetEnabledCheckpoints(ctx context.Context) (int, error) {
	chats, err := s.files.LoadChats(ctx)
	if err != nil {
		return 0, fmt.Errorf("load chats: %w", err)
	}
	checkpoints, err := s.files.LoadCheckpoints(ctx)
	if err != nil {
		return 0, fmt.Errorf("load checkpoints: %w", err)
	}

	var reset int
	for _, chat := range chats {
		if !chat.Enabled || storage.IsPersonalChat(chat) {
			continue
		}
		delete(checkpoints, storage.CheckpointKey(chat))
		reset++
	}
	if reset == 0 {
		return 0, nil
	}
	if err := s.files.SaveCheckpoints(ctx, checkpoints); err != nil {
		return reset, fmt.Errorf("save checkpoints: %w", err)
	}

	return reset, nil
}

func (s Service) FindChat(ctx context.Context, chatType string, id int64) (storage.Chat, error) {
	chats, err := s.files.LoadChats(ctx)
	if err != nil {
		return storage.Chat{}, err
	}
	for _, chat := range chats {
		if chat.Type == chatType && chat.ID == id {
			return chat, nil
		}
	}

	return storage.Chat{}, fmt.Errorf("chat not found type=%s id=%d", chatType, id)
}

func (s Service) Keywords(ctx context.Context) ([]string, error) {
	return s.keywords.Load(ctx)
}

func (s Service) AddKeyword(ctx context.Context, keyword string) (bool, error) {
	return s.keywords.Add(ctx, keyword)
}

func (s Service) RemoveKeyword(ctx context.Context, keyword string) (bool, error) {
	return s.keywords.Remove(ctx, keyword)
}

func (s Service) ParseOnce(ctx context.Context, options ParseOptions, onMatch func(context.Context, storage.Match) error) (ParseSummary, error) {
	keywords, err := s.keywords.Load(ctx)
	if err != nil {
		return ParseSummary{}, fmt.Errorf("load keywords: %w", err)
	}

	matcher := msgparser.NewMatcher(keywords)
	if matcher.Empty() {
		return ParseSummary{}, fmt.Errorf("keywords list is empty")
	}

	chats, err := s.files.LoadChats(ctx)
	if err != nil {
		return ParseSummary{}, fmt.Errorf("load chats: %w", err)
	}
	checkpoints, err := s.files.LoadCheckpoints(ctx)
	if err != nil {
		return ParseSummary{}, fmt.Errorf("load checkpoints: %w", err)
	}

	var summary ParseSummary
	for _, chat := range chats {
		if !chat.Enabled {
			continue
		}
		summary.Enabled++

		if err := storage.ValidateChat(chat); err != nil {
			summary.Failed++
			log.Printf("chat config invalid %s err=%v", storage.ChatLogValue(chat), err)
			continue
		}

		matches, err := s.parseChat(ctx, checkpoints, chat, matcher, options.Limit, onMatch)
		if err != nil {
			summary.Failed++
			log.Printf("chat parse failed %s checkpoint=%d err=%v", storage.ChatLogValue(chat), checkpoints[storage.CheckpointKey(chat)], err)
			continue
		}
		summary.Matches += matches

		if err := s.files.SaveCheckpoints(ctx, checkpoints); err != nil {
			return summary, fmt.Errorf("save checkpoints after %s: %w", storage.ChatLogValue(chat), err)
		}
	}

	return summary, nil
}

func (s Service) ReparseEnabled(ctx context.Context, options ParseOptions, onMatch func(context.Context, storage.Match) error) (ParseSummary, error) {
	keywords, err := s.keywords.Load(ctx)
	if err != nil {
		return ParseSummary{}, fmt.Errorf("load keywords: %w", err)
	}

	matcher := msgparser.NewMatcher(keywords)
	if matcher.Empty() {
		return ParseSummary{}, fmt.Errorf("keywords list is empty")
	}

	chats, err := s.files.LoadChats(ctx)
	if err != nil {
		return ParseSummary{}, fmt.Errorf("load chats: %w", err)
	}

	var summary ParseSummary
	for _, chat := range chats {
		if !chat.Enabled {
			continue
		}
		summary.Enabled++

		matches, err := s.parseChatFrom(ctx, chat, matcher, options.Limit, 0, false, nil, onMatch)
		if err != nil {
			summary.Failed++
			log.Printf("chat reparse failed %s err=%v", storage.ChatLogValue(chat), err)
			continue
		}
		summary.Matches += matches
	}

	return summary, nil
}

func (s Service) ReparseChat(ctx context.Context, chatType string, id int64, options ParseOptions, onMatch func(context.Context, storage.Match) error) (ParseSummary, error) {
	keywords, err := s.keywords.Load(ctx)
	if err != nil {
		return ParseSummary{}, fmt.Errorf("load keywords: %w", err)
	}

	matcher := msgparser.NewMatcher(keywords)
	if matcher.Empty() {
		return ParseSummary{}, fmt.Errorf("keywords list is empty")
	}

	chat, err := s.FindChat(ctx, chatType, id)
	if err != nil {
		return ParseSummary{}, err
	}

	summary := ParseSummary{Enabled: 1}
	matches, err := s.parseChatFrom(ctx, chat, matcher, options.Limit, 0, false, nil, onMatch)
	if err != nil {
		summary.Failed = 1
		return summary, err
	}
	summary.Matches = matches

	return summary, nil
}

func (s Service) parseChat(
	ctx context.Context,
	checkpoints map[string]int,
	chat storage.Chat,
	matcher msgparser.Matcher,
	limit int,
	onMatch func(context.Context, storage.Match) error,
) (int, error) {
	key := storage.CheckpointKey(chat)
	checkpoint := checkpoints[key]
	return s.parseChatFrom(ctx, chat, matcher, limit, checkpoint, true, func(maxID int) {
		if maxID > checkpoint {
			checkpoints[key] = maxID
		}
	}, onMatch)
}

func (s Service) parseChatFrom(
	ctx context.Context,
	chat storage.Chat,
	matcher msgparser.Matcher,
	limit int,
	minID int,
	updateCheckpoint bool,
	setCheckpoint func(int),
	onMatch func(context.Context, storage.Match) error,
) (int, error) {
	messages, err := s.tg.HistoryAfter(ctx, telegram.PeerRef{
		ID:         chat.ID,
		Type:       chat.Type,
		AccessHash: chat.AccessHash,
	}, minID, limit)
	if err != nil {
		return 0, fmt.Errorf("get history: %w", err)
	}

	sort.Slice(messages, func(i int, j int) bool {
		return messages[i].ID < messages[j].ID
	})

	var matched int
	maxID := minID
	for _, message := range messages {
		if message.ID <= minID {
			continue
		}
		if message.ID > maxID {
			maxID = message.ID
		}

		text := oneLine(message.Text)
		if text == "" {
			continue
		}

		match, ok := matcher.Match(text)
		if !ok {
			continue
		}

		result := storage.Match{
			ChatID:       chat.ID,
			ChatType:     chat.Type,
			ChatTitle:    chat.Title,
			ChatUsername: chat.Username,
			MessageID:    message.ID,
			Keyword:      match.Keyword,
			Text:         text,
			Date:         message.Date,
			ParsedAt:     time.Now(),
			Views:        message.Views,
		}
		if err := s.files.AppendMatch(ctx, result); err != nil {
			return matched, fmt.Errorf("append match message_id=%d keyword=%q: %w", message.ID, match.Keyword, err)
		}
		if onMatch != nil {
			if err := onMatch(ctx, result); err != nil {
				log.Printf("notify match failed chat_id=%d message_id=%d err=%v", result.ChatID, result.MessageID, err)
			}
		}

		matched++
	}

	if updateCheckpoint && setCheckpoint != nil && maxID > minID {
		setCheckpoint(maxID)
	}

	return matched, nil
}

func oneLine(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.TrimSpace(value)
}
