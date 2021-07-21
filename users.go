package main

import (
	"time"

	"github.com/whyrusleeping/estuary/util"
	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	UUID     string `gorm:"unique"`
	Username string `gorm:"unique"`
	PassHash string

	UserEmail string

	Address util.DbAddr

	Perm  int
	Flags int
}

type AuthToken struct {
	gorm.Model
	Token  string `gorm:"unique"`
	User   uint
	Expiry time.Time
}

type InviteCode struct {
	gorm.Model
	Code      string `gorm:"unique"`
	CreatedBy uint
	ClaimedBy uint
}
