package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	UserStatusPending = "pending"
	UserStatusActive  = "active"
	UserStatusBlocked = "blocked"
)

type User struct {
	TelegramID int64     `json:"telegram_id"`
	Username   string    `json:"username,omitempty"`
	FirstName  string    `json:"first_name,omitempty"`
	LastName   string    `json:"last_name,omitempty"`
	Role       string    `json:"role"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type UsersFile struct {
	Users []User `json:"users"`
}

type UserStore struct {
	path string
}

func NewUserStore(path string) UserStore {
	return UserStore{path: path}
}

func (s UserStore) Load(ctx context.Context) (UsersFile, error) {
	if err := ctx.Err(); err != nil {
		return UsersFile{}, err
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return UsersFile{}, nil
	}
	if err != nil {
		return UsersFile{}, err
	}
	if len(data) == 0 {
		return UsersFile{}, nil
	}

	var users UsersFile
	if err := json.Unmarshal(data, &users); err != nil {
		return UsersFile{}, err
	}

	return users, nil
}

func (s UserStore) Save(ctx context.Context, users UsersFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureDir(s.path); err != nil {
		return err
	}

	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return os.WriteFile(s.path, data, 0600)
}

func (s UserStore) EnsureAdmins(ctx context.Context, ids []int64) error {
	users, err := s.Load(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	for _, id := range ids {
		if id == 0 {
			continue
		}

		index := userIndex(users.Users, id)
		if index < 0 {
			users.Users = append(users.Users, User{
				TelegramID: id,
				Role:       RoleAdmin,
				Status:     UserStatusActive,
				CreatedAt:  now,
				UpdatedAt:  now,
			})
			continue
		}

		users.Users[index].Role = RoleAdmin
		users.Users[index].Status = UserStatusActive
		users.Users[index].UpdatedAt = now
	}

	return s.Save(ctx, users)
}

func (s UserStore) UpsertPending(ctx context.Context, user User) (User, bool, error) {
	users, err := s.Load(ctx)
	if err != nil {
		return User{}, false, err
	}

	now := time.Now()
	index := userIndex(users.Users, user.TelegramID)
	if index >= 0 {
		existing := users.Users[index]
		existing.Username = user.Username
		existing.FirstName = user.FirstName
		existing.LastName = user.LastName
		existing.UpdatedAt = now
		users.Users[index] = existing
		if err := s.Save(ctx, users); err != nil {
			return User{}, false, err
		}
		return existing, false, nil
	}

	user.Role = RoleUser
	user.Status = UserStatusPending
	user.CreatedAt = now
	user.UpdatedAt = now
	users.Users = append(users.Users, user)

	if err := s.Save(ctx, users); err != nil {
		return User{}, false, err
	}

	return user, true, nil
}

func (s UserStore) SetStatus(ctx context.Context, telegramID int64, status string) (User, error) {
	users, err := s.Load(ctx)
	if err != nil {
		return User{}, err
	}

	index := userIndex(users.Users, telegramID)
	if index < 0 {
		return User{}, errors.New("user not found")
	}

	users.Users[index].Status = status
	users.Users[index].UpdatedAt = time.Now()
	if status == UserStatusActive && users.Users[index].Role == "" {
		users.Users[index].Role = RoleUser
	}

	if err := s.Save(ctx, users); err != nil {
		return User{}, err
	}

	return users.Users[index], nil
}

func (s UserStore) SetRole(ctx context.Context, telegramID int64, role string) (User, error) {
	users, err := s.Load(ctx)
	if err != nil {
		return User{}, err
	}

	index := userIndex(users.Users, telegramID)
	if index < 0 {
		return User{}, errors.New("user not found")
	}

	users.Users[index].Role = role
	users.Users[index].UpdatedAt = time.Now()
	if err := s.Save(ctx, users); err != nil {
		return User{}, err
	}

	return users.Users[index], nil
}

func (s UserStore) Get(ctx context.Context, telegramID int64) (User, bool, error) {
	users, err := s.Load(ctx)
	if err != nil {
		return User{}, false, err
	}

	index := userIndex(users.Users, telegramID)
	if index < 0 {
		return User{}, false, nil
	}

	return users.Users[index], true, nil
}

func (s UserStore) ActiveUsers(ctx context.Context) ([]User, error) {
	users, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}

	var active []User
	for _, user := range users.Users {
		if user.Status == UserStatusActive {
			active = append(active, user)
		}
	}

	return active, nil
}

func (s UserStore) AdminIDs(ctx context.Context) ([]int64, error) {
	users, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}

	var ids []int64
	for _, user := range users.Users {
		if user.Role == RoleAdmin && user.Status == UserStatusActive {
			ids = append(ids, user.TelegramID)
		}
	}

	return ids, nil
}

func IsActive(user User) bool {
	return user.Status == UserStatusActive
}

func IsAdmin(user User) bool {
	return user.Status == UserStatusActive && user.Role == RoleAdmin
}

func userIndex(users []User, telegramID int64) int {
	return slices.IndexFunc(users, func(user User) bool {
		return user.TelegramID == telegramID
	})
}
