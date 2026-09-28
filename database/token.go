package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrTokenNotFound = errors.New("token not found")

// CreateToken stores a new token and sets its ID.
func (store *Store) CreateToken(ctx context.Context, token *models.Token) error {
	if err := store.withContext(ctx).Create(token).Error; err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}

// GetTokenByHash returns the token of a kind with the given hash, or
// ErrTokenNotFound. Expiry is the caller's to check.
func (store *Store) GetTokenByHash(ctx context.Context, kind, hash string) (models.Token, error) {
	var token models.Token
	err := store.withContext(ctx).Where("kind = ? AND hash = ?", kind, hash).Take(&token).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Token{}, ErrTokenNotFound
	}
	if err != nil {
		return models.Token{}, fmt.Errorf("get token: %w", err)
	}
	return token, nil
}

// TouchToken records that a token was used, and moves its expiry.
func (store *Store) TouchToken(ctx context.Context, tokenID uuid.UUID, usedAt, expiresAt time.Time) error {
	result := store.withContext(ctx).Model(&models.Token{}).Where("id = ?", tokenID).
		Updates(map[string]any{"last_used_at": usedAt, "expires_at": expiresAt})
	if result.Error != nil {
		return fmt.Errorf("touch token: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// DeleteToken removes one token; a token already gone is not an error.
func (store *Store) DeleteToken(ctx context.Context, tokenID uuid.UUID) error {
	if err := store.withContext(ctx).Delete(&models.Token{}, "id = ?", tokenID).Error; err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}

// DeleteUserTokens removes every token of a user, of any kind, and returns
// how many there were: signing a user out everywhere.
func (store *Store) DeleteUserTokens(ctx context.Context, userID uuid.UUID) (int64, error) {
	result := store.withContext(ctx).Delete(&models.Token{}, "user_id = ?", userID)
	if result.Error != nil {
		return 0, fmt.Errorf("delete tokens: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// DeleteExpiredTokens removes tokens that expired before now.
func (store *Store) DeleteExpiredTokens(ctx context.Context, now time.Time) (int64, error) {
	result := store.withContext(ctx).Delete(&models.Token{}, "expires_at < ?", now)
	if result.Error != nil {
		return 0, fmt.Errorf("delete expired tokens: %w", result.Error)
	}
	return result.RowsAffected, nil
}
