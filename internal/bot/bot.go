package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"parserTgChat/internal/app"
	"parserTgChat/internal/storage"
)

type Config struct {
	Token             string
	AdminIDs          []int64
	UsersPath         string
	ParseLimit        int
	AutoParseEnabled  bool
	AutoParseInterval time.Duration
}

type Bot struct {
	api               *apiClient
	users             storage.UserStore
	app               app.Service
	adminIDs          []int64
	parseLimit        int
	states            map[int64]string
	autoMu            sync.RWMutex
	autoParseEnabled  bool
	autoParseInterval time.Duration
	parseLock         chan struct{}
}

func New(config Config, service app.Service) *Bot {
	if config.AutoParseInterval <= 0 {
		config.AutoParseInterval = time.Hour
	}

	return &Bot{
		api:               newAPIClient(config.Token),
		users:             storage.NewUserStore(config.UsersPath),
		app:               service,
		adminIDs:          config.AdminIDs,
		parseLimit:        config.ParseLimit,
		states:            map[int64]string{},
		autoParseEnabled:  config.AutoParseEnabled,
		autoParseInterval: config.AutoParseInterval,
		parseLock:         make(chan struct{}, 1),
	}
}

func (b *Bot) Run(ctx context.Context) error {
	if err := b.users.EnsureAdmins(ctx, b.adminIDs); err != nil {
		return fmt.Errorf("ensure bot admins: %w", err)
	}

	go b.autoParseLoop(ctx)

	log.Printf("bot polling started")
	var offset int
	for {
		updates, err := b.api.getUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("get updates failed: %v", err)
			sleep(ctx, 3*time.Second)
			continue
		}

		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			if err := b.handleUpdate(ctx, update); err != nil {
				log.Printf("handle update failed update_id=%d err=%v", update.UpdateID, err)
			}
		}
	}
}

func (b *Bot) handleUpdate(ctx context.Context, update Update) error {
	if update.CallbackQuery != nil {
		return b.handleCallback(ctx, *update.CallbackQuery)
	}
	if update.Message != nil {
		return b.handleMessage(ctx, *update.Message)
	}

	return nil
}

func (b *Bot) handleMessage(ctx context.Context, message Message) error {
	if message.From == nil {
		return nil
	}

	user, allowed, err := b.authorize(ctx, *message.From, message.Chat.ID)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	text := strings.TrimSpace(message.Text)
	if text == "" {
		return b.sendMainMenu(ctx, message.Chat.ID, storage.IsAdmin(user))
	}
	if handled, err := b.handleReplyMenuButton(ctx, message.Chat.ID, user, text); handled || err != nil {
		return err
	}
	if !strings.HasPrefix(text, "/") {
		if handled, err := b.handleStateMessage(ctx, message.Chat.ID, message.From.ID, text); handled || err != nil {
			return err
		}
	}

	command, args := splitCommand(text)
	switch command {
	case "/start", "/help", "/menu":
		return b.sendMainMenu(ctx, message.Chat.ID, storage.IsAdmin(user))
	case "/status":
		return b.sendStatus(ctx, message.Chat.ID)
	case "/chats":
		return b.sendChats(ctx, message.Chat.ID)
	case "/enable_chat":
		return b.setChatEnabled(ctx, message.Chat.ID, args, true)
	case "/disable_chat":
		return b.setChatEnabled(ctx, message.Chat.ID, args, false)
	case "/keywords":
		return b.sendKeywords(ctx, message.Chat.ID)
	case "/add_keyword":
		return b.addKeyword(ctx, message.Chat.ID, args)
	case "/remove_keyword":
		return b.removeKeyword(ctx, message.Chat.ID, args)
	case "/parse_now":
		return b.parseNow(ctx, message.Chat.ID)
	case "/start_parser":
		return b.setAutoParse(ctx, message.Chat.ID, true)
	case "/stop_parser":
		return b.setAutoParse(ctx, message.Chat.ID, false)
	case "/auto_status":
		return b.sendAutoStatus(ctx, message.Chat.ID)
	case "/sync_chats":
		return b.syncChats(ctx, message.Chat.ID)
	case "/folders":
		return b.sendFoldersMenu(ctx, message.Chat.ID)
	case "/requests":
		if !storage.IsAdmin(user) {
			return b.api.sendMessage(ctx, message.Chat.ID, "Недостаточно прав.", nil)
		}
		return b.sendUsers(ctx, message.Chat.ID, storage.UserStatusPending)
	case "/users":
		if !storage.IsAdmin(user) {
			return b.api.sendMessage(ctx, message.Chat.ID, "Недостаточно прав.", nil)
		}
		return b.sendUsers(ctx, message.Chat.ID, "")
	case "/approve":
		if !storage.IsAdmin(user) {
			return b.api.sendMessage(ctx, message.Chat.ID, "Недостаточно прав.", nil)
		}
		return b.setUserStatusFromArgs(ctx, message.Chat.ID, args, storage.UserStatusActive)
	case "/reject", "/block":
		if !storage.IsAdmin(user) {
			return b.api.sendMessage(ctx, message.Chat.ID, "Недостаточно прав.", nil)
		}
		return b.setUserStatusFromArgs(ctx, message.Chat.ID, args, storage.UserStatusBlocked)
	case "/unblock":
		if !storage.IsAdmin(user) {
			return b.api.sendMessage(ctx, message.Chat.ID, "Недостаточно прав.", nil)
		}
		return b.setUserStatusFromArgs(ctx, message.Chat.ID, args, storage.UserStatusActive)
	case "/make_admin":
		if !storage.IsAdmin(user) {
			return b.api.sendMessage(ctx, message.Chat.ID, "Недостаточно прав.", nil)
		}
		return b.setUserRoleFromArgs(ctx, message.Chat.ID, args, storage.RoleAdmin)
	default:
		return b.api.sendMessage(ctx, message.Chat.ID, "Неизвестная команда. /help", nil)
	}
}

func (b *Bot) authorize(ctx context.Context, from User, chatID int64) (storage.User, bool, error) {
	user, ok, err := b.users.Get(ctx, from.ID)
	if err != nil {
		return storage.User{}, false, err
	}
	if ok {
		switch user.Status {
		case storage.UserStatusActive:
			return user, true, nil
		case storage.UserStatusPending:
			return user, false, b.api.sendMessage(ctx, chatID, "Запрос на доступ уже отправлен администратору.", nil)
		case storage.UserStatusBlocked:
			return user, false, b.api.sendMessage(ctx, chatID, "Доступ к боту заблокирован.", nil)
		}
	}

	pending, created, err := b.users.UpsertPending(ctx, storage.User{
		TelegramID: from.ID,
		Username:   from.Username,
		FirstName:  from.FirstName,
		LastName:   from.LastName,
	})
	if err != nil {
		return storage.User{}, false, err
	}
	if created {
		if err := b.notifyAdminsAccessRequest(ctx, pending); err != nil {
			log.Printf("notify admins access request failed user_id=%d err=%v", from.ID, err)
		}
	}

	return pending, false, b.api.sendMessage(ctx, chatID, "Запрос на доступ отправлен администратору.", nil)
}

func (b *Bot) handleCallback(ctx context.Context, query CallbackQuery) error {
	if query.From.ID == 0 {
		return nil
	}

	user, ok, err := b.users.Get(ctx, query.From.ID)
	if err != nil {
		return err
	}
	if !ok || !storage.IsActive(user) {
		return b.api.answerCallbackQuery(ctx, query.ID, "Нет доступа")
	}

	if err := b.api.answerCallbackQuery(ctx, query.ID, "OK"); err != nil {
		log.Printf("answer callback failed: %v", err)
	}

	if strings.HasPrefix(query.Data, "menu:") {
		if query.Message == nil {
			return nil
		}
		return b.handleMenuCallback(ctx, query.Message.Chat.ID, query.From.ID, strings.TrimPrefix(query.Data, "menu:"), storage.IsAdmin(user))
	}
	if strings.HasPrefix(query.Data, "chat:") {
		if query.Message == nil {
			return nil
		}
		return b.handleChatCallback(ctx, query.Message.Chat.ID, strings.TrimPrefix(query.Data, "chat:"))
	}
	if strings.HasPrefix(query.Data, "reparse:") {
		if query.Message == nil {
			return nil
		}
		return b.handleReparseCallback(ctx, query.Message.Chat.ID, strings.TrimPrefix(query.Data, "reparse:"))
	}
	if strings.HasPrefix(query.Data, "reset:") {
		if query.Message == nil {
			return nil
		}
		return b.handleResetCallback(ctx, query.Message.Chat.ID, strings.TrimPrefix(query.Data, "reset:"))
	}
	if strings.HasPrefix(query.Data, "kw:") {
		if query.Message == nil {
			return nil
		}
		return b.handleKeywordCallback(ctx, query.Message.Chat.ID, query.From.ID, strings.TrimPrefix(query.Data, "kw:"))
	}
	if strings.HasPrefix(query.Data, "folder:") {
		if query.Message == nil {
			return nil
		}
		return b.handleFolderCallback(ctx, query.Message.Chat.ID, strings.TrimPrefix(query.Data, "folder:"))
	}

	if !storage.IsAdmin(user) {
		return b.api.answerCallbackQuery(ctx, query.ID, "Недостаточно прав")
	}

	action, id, ok := parseAccessCallback(query.Data)
	if !ok {
		return nil
	}

	if action == "make_admin" {
		if query.Message == nil {
			return b.makeAdmin(ctx, 0, id)
		}
		return b.makeAdmin(ctx, query.Message.Chat.ID, id)
	}

	status := storage.UserStatusBlocked
	text := "Доступ отклонен"
	if action == "approve" {
		status = storage.UserStatusActive
		text = "Доступ одобрен"
	}

	updatedUser, err := b.users.SetStatus(ctx, id, status)
	if err != nil {
		return err
	}
	if query.Message != nil {
		_ = b.api.sendMessage(ctx, query.Message.Chat.ID, fmt.Sprintf("%s: %s", text, formatUser(updatedUser)), nil)
	}
	_ = b.api.sendMessage(ctx, id, text+".", nil)

	return nil
}

func (b *Bot) sendHelp(ctx context.Context, chatID int64, admin bool) error {
	text := strings.Join([]string{
		"/status - статус парсера",
		"/chats - список чатов",
		"/enable_chat <type> <id> - включить чат",
		"/disable_chat <type> <id> - выключить чат",
		"/keywords - ключевые слова",
		"/add_keyword <слово> - добавить ключ",
		"/remove_keyword <слово> - удалить ключ",
		"/parse_now - запустить парсинг сейчас",
		"/start_parser - включить автопарсинг",
		"/stop_parser - выключить автопарсинг",
		"/auto_status - статус автопарсинга",
		"/sync_chats - обновить список чатов",
		"/folders - выбрать папку Telegram для синхронизации",
	}, "\n")
	if admin {
		text += "\n\nadmin:\n" + strings.Join([]string{
			"/requests - заявки доступа",
			"/users - пользователи",
			"/approve <telegram_id>",
			"/reject <telegram_id>",
			"/block <telegram_id>",
			"/unblock <telegram_id>",
			"/make_admin <telegram_id> - выдать полную админку",
		}, "\n")
	}

	return b.api.sendMessage(ctx, chatID, text, nil)
}

func (b *Bot) sendMainMenu(ctx context.Context, chatID int64, admin bool) error {
	if err := b.sendReplyMenu(ctx, chatID, admin); err != nil {
		return err
	}

	rows := [][]InlineKeyboardButton{
		{
			{Text: "📊 Статус", CallbackData: "menu:status"},
			{Text: "💬 Чаты", CallbackData: "menu:chats"},
		},
		{
			{Text: "🔎 Ключевые", CallbackData: "menu:keywords"},
			{Text: "▶️ Парсинг", CallbackData: "menu:parse"},
		},
		{
			{Text: "📁 Папки", CallbackData: "menu:folders"},
		},
	}
	if admin {
		rows = append(rows, []InlineKeyboardButton{
			{Text: "👥 Доступ", CallbackData: "menu:access"},
			{Text: "🔄 Синхронизация", CallbackData: "menu:sync"},
		})
	} else {
		rows = append(rows, []InlineKeyboardButton{
			{Text: "🔄 Синхронизация", CallbackData: "menu:sync"},
		})
	}
	rows = append(rows, []InlineKeyboardButton{
		{Text: replyHideButton, CallbackData: "menu:hide_reply_menu"},
	})

	return b.api.sendMessage(ctx, chatID, "💻 Выберите раздел", &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) handleMenuCallback(ctx context.Context, chatID int64, userID int64, section string, admin bool) error {
	switch section {
	case "home":
		return b.sendMainMenu(ctx, chatID, admin)
	case "show_reply_menu":
		return b.sendMainMenu(ctx, chatID, admin)
	case "hide_reply_menu":
		return b.hideReplyMenu(ctx, chatID)
	case "status":
		return b.sendStatus(ctx, chatID)
	case "chats":
		return b.sendChatsMenu(ctx, chatID)
	case "keywords":
		return b.sendKeywordsMenu(ctx, chatID)
	case "parse":
		return b.sendParseMenu(ctx, chatID)
	case "parse_now":
		return b.parseNow(ctx, chatID)
	case "reparse_all":
		return b.reparseAll(ctx, chatID)
	case "auto_start":
		return b.setAutoParse(ctx, chatID, true)
	case "auto_stop":
		return b.setAutoParse(ctx, chatID, false)
	case "auto_status":
		return b.sendAutoStatus(ctx, chatID)
	case "access":
		if !admin {
			return b.api.sendMessage(ctx, chatID, "Недостаточно прав.", nil)
		}
		return b.sendAccessMenu(ctx, chatID)
	case "requests":
		if !admin {
			return b.api.sendMessage(ctx, chatID, "Недостаточно прав.", nil)
		}
		return b.sendUsers(ctx, chatID, storage.UserStatusPending)
	case "users":
		if !admin {
			return b.api.sendMessage(ctx, chatID, "Недостаточно прав.", nil)
		}
		return b.sendUsers(ctx, chatID, "")
	case "sync":
		return b.syncChats(ctx, chatID)
	case "folders":
		return b.sendFoldersMenu(ctx, chatID)
	default:
		return b.sendMainMenu(ctx, chatID, admin)
	}
}

func (b *Bot) handleReplyMenuButton(ctx context.Context, chatID int64, user storage.User, text string) (bool, error) {
	switch strings.TrimSpace(text) {
	case replyStartButton:
		return true, b.sendMainMenu(ctx, chatID, storage.IsAdmin(user))
	case replyStatusButton:
		return true, b.sendStatus(ctx, chatID)
	case replyChatsButton:
		return true, b.sendChatsMenu(ctx, chatID)
	case replyKeywordsButton:
		return true, b.sendKeywordsMenu(ctx, chatID)
	case replyParseButton:
		return true, b.sendParseMenu(ctx, chatID)
	case replySyncButton:
		return true, b.syncChats(ctx, chatID)
	case replyFoldersButton:
		return true, b.sendFoldersMenu(ctx, chatID)
	case replyAccessButton:
		if !storage.IsAdmin(user) {
			return true, b.api.sendMessage(ctx, chatID, "Недостаточно прав.", nil)
		}
		return true, b.sendAccessMenu(ctx, chatID)
	case replyHideButton:
		return true, b.hideReplyMenu(ctx, chatID)
	default:
		return false, nil
	}
}

func (b *Bot) sendReplyMenu(ctx context.Context, chatID int64, admin bool) error {
	return b.api.sendMessage(ctx, chatID, "Меню открыто.", mainReplyKeyboard(admin))
}

func (b *Bot) hideReplyMenu(ctx context.Context, chatID int64) error {
	if err := b.api.sendMessage(ctx, chatID, "Меню скрыто.", &ReplyKeyboardRemove{RemoveKeyboard: true}); err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, "Чтобы вернуть меню, нажмите кнопку ниже.", openReplyMenuNav())
}

func (b *Bot) sendFoldersMenu(ctx context.Context, chatID int64) error {
	folders, err := b.app.Folders(ctx)
	if err != nil {
		return err
	}

	rows := [][]InlineKeyboardButton{
		{
			{Text: "🌐 Все чаты", CallbackData: "folder:all"},
		},
	}
	for _, folder := range folders {
		rows = append(rows, []InlineKeyboardButton{
			{
				Text:         truncate(fmt.Sprintf("📁 %s (ID %d)", folder.Title, folder.ID), 55),
				CallbackData: fmt.Sprintf("folder:sync:%d", folder.ID),
			},
		})
	}
	rows = append(rows, navRows()...)

	text := "📁 Папки Telegram\n\nВыберите папку, из которой нужно добавить чаты."
	if len(folders) == 0 {
		text += "\n\nПапки не найдены. Можно синхронизировать все чаты."
	}

	return b.api.sendMessage(ctx, chatID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) handleFolderCallback(ctx context.Context, chatID int64, data string) error {
	if data == "all" {
		return b.syncChats(ctx, chatID)
	}

	parts := strings.Split(data, ":")
	if len(parts) != 2 || parts[0] != "sync" {
		return b.api.sendMessage(ctx, chatID, "Неизвестное действие с папкой.", nil)
	}

	folderID, err := strconv.Atoi(parts[1])
	if err != nil || folderID <= 0 {
		return b.api.sendMessage(ctx, chatID, "Некорректный ID папки.", nil)
	}

	count, err := b.app.SyncChatsInFolder(ctx, folderID, 5000)
	if err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Синхронизировано чатов из папки ID %d: %d", folderID, count), mainNav())
}

func (b *Bot) sendChatsMenu(ctx context.Context, chatID int64) error {
	chats, err := b.app.Chats(ctx)
	if err != nil {
		return err
	}
	if len(chats) == 0 {
		return b.api.sendMessage(ctx, chatID, "Список чатов пуст. Выполните синхронизацию.", mainNav())
	}

	rows := make([][]InlineKeyboardButton, 0, len(chats)+len(navRows()))
	for _, chat := range chats {
		if storage.IsPersonalChat(chat) {
			continue
		}
		mark := "➕"
		if chat.Enabled {
			mark = "✅"
		}
		title := chat.Title
		if title == "" {
			title = chat.Username
		}
		rows = append(rows, []InlineKeyboardButton{{
			Text:         truncate(fmt.Sprintf("%s %s", mark, title), 45),
			CallbackData: fmt.Sprintf("chat:toggle:%s:%d", chat.Type, chat.ID),
		}})
	}
	if len(rows) == 0 {
		return b.api.sendMessage(ctx, chatID, "Нет групп, супергрупп или каналов. Выполните синхронизацию.", mainNav())
	}
	rows = append(rows, navRows()...)

	return b.api.sendMessage(ctx, chatID, "💬 Чаты для парсинга", &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) handleChatCallback(ctx context.Context, chatID int64, data string) error {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "toggle" {
		return b.api.sendMessage(ctx, chatID, "Неизвестное действие с чатом.", nil)
	}

	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return err
	}

	chats, err := b.app.Chats(ctx)
	if err != nil {
		return err
	}
	for _, chat := range chats {
		if chat.Type == parts[1] && chat.ID == id {
			_, err := b.app.SetChatEnabled(ctx, parts[1], id, !chat.Enabled)
			if err != nil {
				return err
			}
			return b.sendChatsMenu(ctx, chatID)
		}
	}

	return b.api.sendMessage(ctx, chatID, "Чат не найден.", nil)
}

func (b *Bot) handleReparseCallback(ctx context.Context, chatID int64, data string) error {
	parts := strings.Split(data, ":")
	if len(parts) == 1 && parts[0] == "all" {
		return b.reparseAll(ctx, chatID)
	}
	if len(parts) != 4 || parts[0] != "chat" {
		return b.api.sendMessage(ctx, chatID, "Неизвестное действие перепроверки.", nil)
	}

	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return err
	}
	limit, err := strconv.Atoi(parts[3])
	if err != nil {
		return err
	}

	return b.reparseChat(ctx, chatID, parts[1], id, limit)
}

func (b *Bot) handleResetCallback(ctx context.Context, chatID int64, data string) error {
	if data == "enabled" {
		return b.resetEnabledCheckpoints(ctx, chatID)
	}

	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "chat" {
		return b.api.sendMessage(ctx, chatID, "Неизвестное действие checkpoint.", nil)
	}

	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return err
	}

	chat, err := b.app.ResetCheckpoint(ctx, parts[1], id)
	if err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Checkpoint сброшен: %s:%d %s", chat.Type, chat.ID, chat.Title), mainNav())
}

func (b *Bot) resetEnabledCheckpoints(ctx context.Context, chatID int64) error {
	count, err := b.app.ResetEnabledCheckpoints(ctx)
	if err != nil {
		return err
	}
	if count == 0 {
		return b.api.sendMessage(ctx, chatID, "Нет выбранных чатов для сброса checkpoint.", mainNav())
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Checkpoint сброшен для выбранных чатов: %d", count), mainNav())
}

func (b *Bot) sendKeywordsMenu(ctx context.Context, chatID int64) error {
	keywords, err := b.app.Keywords(ctx)
	if err != nil {
		return err
	}

	text := "🔎 Ключевые слова"
	if len(keywords) > 0 {
		text += "\n\n" + strings.Join(keywords, "\n")
	}

	rows := [][]InlineKeyboardButton{
		{
			{Text: "➕ Добавить", CallbackData: "kw:add"},
			{Text: "🗑 Удалить", CallbackData: "kw:remove"},
		},
	}
	rows = append(rows, navRows()...)

	return b.api.sendMessage(ctx, chatID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) handleKeywordCallback(ctx context.Context, chatID int64, userID int64, action string) error {
	switch action {
	case "add":
		b.states[userID] = "add_keyword"
		return b.api.sendMessage(ctx, chatID, "Отправь ключевое слово следующим сообщением.", cancelNav())
	case "remove":
		b.states[userID] = "remove_keyword"
		return b.api.sendMessage(ctx, chatID, "Отправь ключевое слово для удаления.", cancelNav())
	default:
		return b.sendKeywordsMenu(ctx, chatID)
	}
}

func (b *Bot) handleStateMessage(ctx context.Context, chatID int64, userID int64, text string) (bool, error) {
	state := b.states[userID]
	if state == "" {
		return false, nil
	}
	delete(b.states, userID)

	switch state {
	case "add_keyword":
		if err := b.addKeyword(ctx, chatID, text); err != nil {
			return true, err
		}
		return true, b.sendKeywordsMenu(ctx, chatID)
	case "remove_keyword":
		if err := b.removeKeyword(ctx, chatID, text); err != nil {
			return true, err
		}
		return true, b.sendKeywordsMenu(ctx, chatID)
	default:
		return false, nil
	}
}

func (b *Bot) sendParseMenu(ctx context.Context, chatID int64) error {
	rows := [][]InlineKeyboardButton{
		{
			{Text: "🚀 Запустить сейчас", CallbackData: "menu:parse_now"},
		},
		{
			{Text: "🔁 Проверить последние 100", CallbackData: "menu:reparse_all"},
		},
		{
			{Text: "♻️ Сбросить checkpoint выбранных", CallbackData: "reset:enabled"},
		},
	}
	autoButton := InlineKeyboardButton{Text: "▶️ Включить авто", CallbackData: "menu:auto_start"}
	if b.isAutoParseEnabled() {
		autoButton = InlineKeyboardButton{Text: "⏸ Выключить авто", CallbackData: "menu:auto_stop"}
	}
	rows = append(rows, []InlineKeyboardButton{
		autoButton,
		{Text: "⏱ Статус авто", CallbackData: "menu:auto_status"},
	})
	rows = append(rows, navRows()...)

	text := fmt.Sprintf("▶️ Управление парсингом\nАвтопарсинг: %s\nИнтервал: %s", b.autoParseStatus(), b.autoParseInterval)
	return b.api.sendMessage(ctx, chatID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) sendAccessMenu(ctx context.Context, chatID int64) error {
	rows := [][]InlineKeyboardButton{
		{
			{Text: "🕓 Заявки", CallbackData: "menu:requests"},
			{Text: "👤 Пользователи", CallbackData: "menu:users"},
		},
	}
	rows = append(rows, navRows()...)

	return b.api.sendMessage(ctx, chatID, "👥 Управление доступом", &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) sendStatus(ctx context.Context, chatID int64) error {
	chats, err := b.app.Chats(ctx)
	if err != nil {
		return err
	}
	keywords, err := b.app.Keywords(ctx)
	if err != nil {
		return err
	}

	var enabled int
	for _, chat := range chats {
		if chat.Enabled {
			enabled++
		}
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf(
		"Чатов всего: %d\nВключено: %d\nКлючевых слов: %d\nАвтопарсинг: %s\nИнтервал: %s",
		len(chats),
		enabled,
		len(keywords),
		b.autoParseStatus(),
		b.autoParseInterval,
	), mainNav())
}

func (b *Bot) sendChats(ctx context.Context, chatID int64) error {
	chats, err := b.app.Chats(ctx)
	if err != nil {
		return err
	}
	if len(chats) == 0 {
		return b.api.sendMessage(ctx, chatID, "Список чатов пуст. Выполните /sync_chats.", nil)
	}

	var builder strings.Builder
	for _, chat := range chats {
		if storage.IsPersonalChat(chat) {
			continue
		}
		mark := "off"
		if chat.Enabled {
			mark = "on"
		}
		builder.WriteString(fmt.Sprintf("%s %s:%d %s", mark, chat.Type, chat.ID, chat.Title))
		if chat.Username != "" {
			builder.WriteString(" @" + chat.Username)
		}
		builder.WriteByte('\n')
	}
	if builder.Len() == 0 {
		builder.WriteString("Нет групп, супергрупп или каналов. Выполните /sync_chats.")
	}

	return b.api.sendMessage(ctx, chatID, builder.String(), nil)
}

func (b *Bot) setChatEnabled(ctx context.Context, chatID int64, args string, enabled bool) error {
	chatType, id, err := parseChatRef(args)
	if err != nil {
		return b.api.sendMessage(ctx, chatID, "Формат: /enable_chat <type> <id>", nil)
	}

	chat, err := b.app.SetChatEnabled(ctx, chatType, id, enabled)
	if err != nil {
		return err
	}

	state := "выключен"
	if enabled {
		state = "включен"
	}
	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Чат %s:%d %s: %s", chat.Type, chat.ID, state, chat.Title), nil)
}

func (b *Bot) sendKeywords(ctx context.Context, chatID int64) error {
	keywords, err := b.app.Keywords(ctx)
	if err != nil {
		return err
	}
	if len(keywords) == 0 {
		return b.api.sendMessage(ctx, chatID, "Ключевых слов пока нет.", nil)
	}

	return b.api.sendMessage(ctx, chatID, strings.Join(keywords, "\n"), nil)
}

func (b *Bot) addKeyword(ctx context.Context, chatID int64, args string) error {
	added, err := b.app.AddKeyword(ctx, args)
	if err != nil {
		return err
	}
	if !added {
		return b.api.sendMessage(ctx, chatID, "Ключевое слово уже есть или пустое.", nil)
	}

	keyboard := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "🔁 Проверить последние 100", CallbackData: "reparse:all"},
			},
			{
				{Text: "🔎 Ключевые", CallbackData: "menu:keywords"},
				{Text: "🏠 Главное", CallbackData: "menu:home"},
			},
		},
	}
	return b.api.sendMessage(ctx, chatID, "Ключевое слово добавлено. Проверить последние сообщения во включенных чатах?", &keyboard)
}

func (b *Bot) removeKeyword(ctx context.Context, chatID int64, args string) error {
	removed, err := b.app.RemoveKeyword(ctx, args)
	if err != nil {
		return err
	}
	if !removed {
		return b.api.sendMessage(ctx, chatID, "Ключевое слово не найдено.", nil)
	}

	return b.api.sendMessage(ctx, chatID, "Ключевое слово удалено.", nil)
}

func (b *Bot) syncChats(ctx context.Context, chatID int64) error {
	count, err := b.app.SyncChats(ctx, 100)
	if err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Синхронизировано чатов: %d", count), nil)
}

func (b *Bot) autoParseLoop(ctx context.Context) {
	log.Printf("auto parser loop started enabled=%t interval=%s", b.isAutoParseEnabled(), b.autoParseInterval)

	ticker := time.NewTicker(b.autoParseInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("auto parser loop stopped")
			return
		case <-ticker.C:
			if !b.isAutoParseEnabled() {
				continue
			}
			b.runAutoParse(ctx)
		}
	}
}

func (b *Bot) runAutoParse(ctx context.Context) {
	if !b.acquireParse() {
		log.Printf("auto parser skipped: parse already running")
		return
	}
	defer b.releaseParse()

	log.Printf("auto parser started")
	summary, err := b.app.ParseOnce(ctx, app.ParseOptions{Limit: b.parseLimit}, b.notifyMatch)
	if err != nil {
		log.Printf("auto parser failed: %v", err)
		if notifyErr := b.notifyAdmins(ctx, fmt.Sprintf("⚠️ Автопарсинг завершился с ошибкой:\n%v", err)); notifyErr != nil {
			log.Printf("notify admins auto parser failed: %v", notifyErr)
		}
		return
	}

	log.Printf("auto parser completed enabled=%d failed=%d matches=%d", summary.Enabled, summary.Failed, summary.Matches)
}

func (b *Bot) acquireParse() bool {
	select {
	case b.parseLock <- struct{}{}:
		return true
	default:
		return false
	}
}

func (b *Bot) releaseParse() {
	select {
	case <-b.parseLock:
	default:
	}
}

func (b *Bot) setAutoParse(ctx context.Context, chatID int64, enabled bool) error {
	b.setAutoParseEnabled(enabled)
	return b.sendAutoStatus(ctx, chatID)
}

func (b *Bot) setAutoParseEnabled(enabled bool) {
	b.autoMu.Lock()
	defer b.autoMu.Unlock()
	b.autoParseEnabled = enabled
	log.Printf("auto parser enabled=%t interval=%s", enabled, b.autoParseInterval)
}

func (b *Bot) isAutoParseEnabled() bool {
	b.autoMu.RLock()
	defer b.autoMu.RUnlock()
	return b.autoParseEnabled
}

func (b *Bot) autoParseStatus() string {
	if b.isAutoParseEnabled() {
		return "включен"
	}
	return "выключен"
}

func (b *Bot) sendAutoStatus(ctx context.Context, chatID int64) error {
	return b.api.sendMessage(ctx, chatID, fmt.Sprintf(
		"Автопарсинг: %s\nИнтервал: %s",
		b.autoParseStatus(),
		b.autoParseInterval,
	), mainNav())
}

func (b *Bot) parseNow(ctx context.Context, chatID int64) error {
	if !b.acquireParse() {
		return b.api.sendMessage(ctx, chatID, "Парсинг уже выполняется.", nil)
	}
	defer b.releaseParse()

	_ = b.api.sendMessage(ctx, chatID, "Парсинг запущен.", nil)
	summary, err := b.app.ParseOnce(ctx, app.ParseOptions{Limit: b.parseLimit}, b.notifyMatch)
	if err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Готово. enabled=%d failed=%d matches=%d", summary.Enabled, summary.Failed, summary.Matches), nil)
}

func (b *Bot) reparseAll(ctx context.Context, chatID int64) error {
	if !b.acquireParse() {
		return b.api.sendMessage(ctx, chatID, "Парсинг уже выполняется.", nil)
	}
	defer b.releaseParse()

	_ = b.api.sendMessage(ctx, chatID, "Перепроверяю последние 100 сообщений во включенных чатах.", nil)
	summary, err := b.app.ReparseEnabled(ctx, app.ParseOptions{Limit: 100}, b.notifyMatch)
	if err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Перепроверка завершена. enabled=%d failed=%d matches=%d", summary.Enabled, summary.Failed, summary.Matches), mainNav())
}

func (b *Bot) reparseChat(ctx context.Context, chatID int64, chatType string, id int64, limit int) error {
	if !b.acquireParse() {
		return b.api.sendMessage(ctx, chatID, "Парсинг уже выполняется.", nil)
	}
	defer b.releaseParse()

	_ = b.api.sendMessage(ctx, chatID, fmt.Sprintf("Перепроверяю %s:%d последние %d сообщений.", chatType, id, limit), nil)
	summary, err := b.app.ReparseChat(ctx, chatType, id, app.ParseOptions{Limit: limit}, b.notifyMatch)
	if err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, fmt.Sprintf("Готово. failed=%d matches=%d", summary.Failed, summary.Matches), mainNav())
}

func (b *Bot) notifyMatch(ctx context.Context, match storage.Match) error {
	users, err := b.users.ActiveUsers(ctx)
	if err != nil {
		return err
	}

	text := formatMatch(match)
	for _, user := range users {
		if err := b.api.sendMessage(ctx, user.TelegramID, text, nil); err != nil {
			log.Printf("send match failed user_id=%d err=%v", user.TelegramID, err)
		}
	}

	return nil
}

func (b *Bot) notifyAdmins(ctx context.Context, text string) error {
	adminIDs, err := b.users.AdminIDs(ctx)
	if err != nil {
		return err
	}

	for _, adminID := range adminIDs {
		if err := b.api.sendMessage(ctx, adminID, text, nil); err != nil {
			log.Printf("send admin notification failed admin_id=%d err=%v", adminID, err)
		}
	}

	return nil
}

func (b *Bot) sendUsers(ctx context.Context, chatID int64, status string) error {
	usersFile, err := b.users.Load(ctx)
	if err != nil {
		return err
	}

	var builder strings.Builder
	var rows [][]InlineKeyboardButton
	for _, user := range usersFile.Users {
		if status != "" && user.Status != status {
			continue
		}
		builder.WriteString(formatUser(user))
		builder.WriteByte('\n')
		if user.Role != storage.RoleAdmin {
			rows = append(rows, []InlineKeyboardButton{{
				Text:         truncate("👑 Сделать админом "+userDisplayName(user), 55),
				CallbackData: fmt.Sprintf("make_admin:%d", user.TelegramID),
			}})
		}
	}
	if builder.Len() == 0 {
		builder.WriteString("Нет пользователей.")
	}
	rows = append(rows, navRows()...)

	return b.api.sendMessage(ctx, chatID, builder.String(), &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) setUserStatusFromArgs(ctx context.Context, chatID int64, args string, status string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil {
		return b.api.sendMessage(ctx, chatID, "Формат: команда <telegram_id>", nil)
	}

	user, err := b.users.SetStatus(ctx, id, status)
	if err != nil {
		return err
	}

	_ = b.api.sendMessage(ctx, id, "Статус доступа изменен: "+status, nil)
	return b.api.sendMessage(ctx, chatID, "Готово: "+formatUser(user), nil)
}

func (b *Bot) setUserRoleFromArgs(ctx context.Context, chatID int64, args string, role string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil {
		return b.api.sendMessage(ctx, chatID, "Формат: команда <telegram_id>", nil)
	}

	if role == storage.RoleAdmin {
		return b.makeAdmin(ctx, chatID, id)
	}

	user, err := b.users.SetRole(ctx, id, role)
	if err != nil {
		return err
	}

	return b.api.sendMessage(ctx, chatID, "Готово: "+formatUser(user), nil)
}

func (b *Bot) makeAdmin(ctx context.Context, chatID int64, telegramID int64) error {
	user, err := b.users.SetStatus(ctx, telegramID, storage.UserStatusActive)
	if err != nil {
		return err
	}
	user, err = b.users.SetRole(ctx, telegramID, storage.RoleAdmin)
	if err != nil {
		return err
	}

	_ = b.api.sendMessage(ctx, telegramID, "Вам выданы права администратора.", nil)
	if chatID == 0 {
		return nil
	}

	return b.api.sendMessage(ctx, chatID, "Полная админка выдана: "+formatUser(user), mainNav())
}

func (b *Bot) notifyAdminsAccessRequest(ctx context.Context, user storage.User) error {
	adminIDs, err := b.users.AdminIDs(ctx)
	if err != nil {
		return err
	}

	text := "Новый запрос доступа:\n" + formatUser(user)
	keyboard := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Approve", CallbackData: fmt.Sprintf("approve:%d", user.TelegramID)},
				{Text: "Reject", CallbackData: fmt.Sprintf("reject:%d", user.TelegramID)},
			},
		},
	}
	for _, adminID := range adminIDs {
		if err := b.api.sendMessage(ctx, adminID, text, &keyboard); err != nil {
			log.Printf("send access request failed admin_id=%d err=%v", adminID, err)
		}
	}

	return nil
}

func splitCommand(text string) (string, string) {
	parts := strings.SplitN(text, " ", 2)
	command := strings.ToLower(strings.TrimSpace(parts[0]))
	if at := strings.Index(command, "@"); at >= 0 {
		command = command[:at]
	}
	if len(parts) == 1 {
		return command, ""
	}

	return command, strings.TrimSpace(parts[1])
}

func parseChatRef(value string) (string, int64, error) {
	parts := strings.Fields(value)
	if len(parts) != 2 {
		return "", 0, errors.New("invalid chat ref")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, err
	}

	return strings.ToLower(parts[0]), id, nil
}

func parseAccessCallback(data string) (string, int64, bool) {
	action, value, ok := strings.Cut(data, ":")
	if !ok {
		return "", 0, false
	}
	if action != "approve" && action != "reject" && action != "make_admin" {
		return "", 0, false
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return "", 0, false
	}

	return action, id, true
}

func formatUser(user storage.User) string {
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if user.Username != "" {
		name += " @" + user.Username
	}
	return fmt.Sprintf("id=%d role=%s status=%s %s", user.TelegramID, user.Role, user.Status, strings.TrimSpace(name))
}

func userDisplayName(user storage.User) string {
	if user.Username != "" {
		return "@" + user.Username
	}
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name != "" {
		return name
	}

	return strconv.FormatInt(user.TelegramID, 10)
}

func formatMatch(match storage.Match) string {
	text := fmt.Sprintf(
		"Найдено: %s\nЧат: %s (%s:%d)\nДата: %s\n\n%s",
		match.Keyword,
		match.ChatTitle,
		match.ChatType,
		match.ChatID,
		match.Date.Format("2006-01-02 15:04:05"),
		truncate(match.Text, 3000),
	)
	if match.ChatUsername != "" {
		text += fmt.Sprintf("\n\nhttps://t.me/%s/%d", match.ChatUsername, match.MessageID)
	}

	return text
}

func truncate(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}

	runes := []rune(value)
	return string(runes[:limit]) + "..."
}

func sleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func mainNav() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: navRows()}
}

func cancelNav() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "🏠 Главное", CallbackData: "menu:home"},
				{Text: "🔎 Ключевые", CallbackData: "menu:keywords"},
			},
		},
	}
}

func openReplyMenuNav() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "📋 Открыть меню", CallbackData: "menu:show_reply_menu"},
			},
		},
	}
}

func navRows() [][]InlineKeyboardButton {
	return [][]InlineKeyboardButton{
		{
			{Text: "🏠 Главное", CallbackData: "menu:home"},
			{Text: "💬 Чаты", CallbackData: "menu:chats"},
		},
		{
			{Text: "🔎 Ключевые", CallbackData: "menu:keywords"},
			{Text: "▶️ Парсинг", CallbackData: "menu:parse"},
		},
	}
}

const (
	replyStartButton    = "▶️ Старт"
	replyStatusButton   = "📊 Статус"
	replyChatsButton    = "💬 Чаты"
	replyKeywordsButton = "🔎 Ключевые"
	replyParseButton    = "▶️ Парсинг"
	replySyncButton     = "🔄 Синхронизация"
	replyFoldersButton  = "📁 Папки"
	replyAccessButton   = "👥 Доступ"
	replyHideButton     = "❌ Скрыть меню"
)

func mainReplyKeyboard(admin bool) *ReplyKeyboardMarkup {
	rows := [][]KeyboardButton{
		{
			{Text: replyStartButton},
			{Text: replyStatusButton},
		},
		{
			{Text: replyChatsButton},
			{Text: replyKeywordsButton},
		},
		{
			{Text: replyParseButton},
			{Text: replySyncButton},
		},
		{
			{Text: replyFoldersButton},
		},
	}
	if admin {
		rows = append(rows, []KeyboardButton{
			{Text: replyAccessButton},
			{Text: replyHideButton},
		})
	} else {
		rows = append(rows, []KeyboardButton{
			{Text: replyHideButton},
		})
	}

	return &ReplyKeyboardMarkup{
		Keyboard:              rows,
		ResizeKeyboard:        true,
		IsPersistent:          true,
		InputFieldPlaceholder: "Выберите раздел",
	}
}

type apiClient struct {
	base string
	http *http.Client
}

func newAPIClient(token string) *apiClient {
	return &apiClient{
		base: "https://api.telegram.org/bot" + token + "/",
		http: &http.Client{Timeout: 40 * time.Second},
	}
}

func (c *apiClient) getUpdates(ctx context.Context, offset int, timeout int) ([]Update, error) {
	values := url.Values{}
	values.Set("timeout", strconv.Itoa(timeout))
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}

	var response APIResponse[[]Update]
	if err := c.get(ctx, "getUpdates?"+values.Encode(), &response); err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, fmt.Errorf("telegram getUpdates failed: %s", response.Description)
	}

	return response.Result, nil
}

func (c *apiClient) sendMessage(ctx context.Context, chatID int64, text string, markup any) error {
	request := SendMessageRequest{
		ChatID: chatID,
		Text:   truncate(text, 3900),
	}
	if markup != nil {
		request.ReplyMarkup = markup
	}

	var response APIResponse[Message]
	if err := c.post(ctx, "sendMessage", request, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("telegram sendMessage failed: %s", response.Description)
	}

	return nil
}

func (c *apiClient) answerCallbackQuery(ctx context.Context, callbackID string, text string) error {
	request := AnswerCallbackQueryRequest{
		CallbackQueryID: callbackID,
		Text:            text,
	}

	var response APIResponse[bool]
	if err := c.post(ctx, "answerCallbackQuery", request, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("telegram answerCallbackQuery failed: %s", response.Description)
	}

	return nil
}

func (c *apiClient) get(ctx context.Context, method string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+method, nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeResponse(resp, target)
}

func (c *apiClient) post(ctx context.Context, method string, body any, target any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeResponse(resp, target)
}

func decodeResponse(resp *http.Response, target any) error {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram http status=%d body=%s", resp.StatusCode, string(data))
	}

	return json.Unmarshal(data, target)
}

type APIResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
}

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type SendMessageRequest struct {
	ChatID      int64  `json:"chat_id"`
	Text        string `json:"text"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

type AnswerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type ReplyKeyboardMarkup struct {
	Keyboard              [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard        bool               `json:"resize_keyboard,omitempty"`
	IsPersistent          bool               `json:"is_persistent,omitempty"`
	InputFieldPlaceholder string             `json:"input_field_placeholder,omitempty"`
}

type KeyboardButton struct {
	Text string `json:"text"`
}

type ReplyKeyboardRemove struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}
