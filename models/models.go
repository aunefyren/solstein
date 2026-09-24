// Package models holds the persisted records shared across Solstein's
// packages. It contains plain structs and their GORM mapping only; queries
// live in the database package.
package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Base is embedded in every persisted model. IDs are UUIDs assigned on
// create, so callers never have to remember to set one. There is no soft
// delete: a removed feed must be re-addable under the same source URL, which a
// soft-deleted row would block through the unique index.
type Base struct {
	ID        uuid.UUID `json:"id" gorm:"type:varchar(36);primaryKey"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BeforeCreate assigns an ID if the record doesn't have one yet.
func (base *Base) BeforeCreate(tx *gorm.DB) error {
	if base.ID == uuid.Nil {
		base.ID = uuid.New()
	}
	return nil
}
