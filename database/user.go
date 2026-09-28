package database

import (
	"context"
	"errors"
	"fmt"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrUserNotFound = errors.New("user not found")
	ErrUserExists   = errors.New("a user with this name already exists")
)

// CreateUser stores a new user and sets its ID. It returns ErrUserExists if
// the username is taken.
func (store *Store) CreateUser(ctx context.Context, user *models.User) error {
	return store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&models.User{}).Where("username = ?", user.Username).Count(&count).Error; err != nil {
			return fmt.Errorf("check for existing user: %w", err)
		}
		if count > 0 {
			return ErrUserExists
		}
		if err := tx.Create(user).Error; err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		return nil
	})
}

// GetUser returns the user with the given ID, or ErrUserNotFound.
func (store *Store) GetUser(ctx context.Context, userID uuid.UUID) (models.User, error) {
	return store.findUser(ctx, "id = ?", userID)
}

// GetUserByUsername returns the user with the given (lower-case) name, or
// ErrUserNotFound.
func (store *Store) GetUserByUsername(ctx context.Context, username string) (models.User, error) {
	return store.findUser(ctx, "username = ?", username)
}

func (store *Store) findUser(ctx context.Context, query string, arg any) (models.User, error) {
	var user models.User
	err := store.withContext(ctx).Where(query, arg).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.User{}, ErrUserNotFound
	}
	if err != nil {
		return models.User{}, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// ListUsers returns every user, by name.
func (store *Store) ListUsers(ctx context.Context) ([]models.User, error) {
	var users []models.User
	if err := store.withContext(ctx).Order("username").Find(&users).Error; err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// UpdateUser saves every field of an existing user, or returns
// ErrUserNotFound.
func (store *Store) UpdateUser(ctx context.Context, user *models.User) error {
	if user.ID == uuid.Nil {
		return ErrUserNotFound
	}
	result := store.withContext(ctx).Model(user).Select("*").Omit("id", "created_at").Updates(user)
	if result.Error != nil {
		return fmt.Errorf("update user: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// DeleteUser removes a user and, through the foreign key, their tokens. It
// returns ErrUserNotFound if there was nothing to delete.
func (store *Store) DeleteUser(ctx context.Context, userID uuid.UUID) error {
	result := store.withContext(ctx).Delete(&models.User{}, "id = ?", userID)
	if result.Error != nil {
		return fmt.Errorf("delete user: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}
