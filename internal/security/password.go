package security

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

const (
	minAdminPasswordBytes = 12
	maxAdminPasswordBytes = 72
	adminPasswordCost     = 12
)

var ErrInvalidAdminPassword = errors.New("管理员口令长度必须为 12 至 72 字节")

func HashAdminPassword(password []byte) (string, error) {
	if len(password) < minAdminPasswordBytes || len(password) > maxAdminPasswordBytes {
		return "", ErrInvalidAdminPassword
	}
	hash, err := bcrypt.GenerateFromPassword(password, adminPasswordCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func VerifyAdminPassword(hash string, password []byte) bool {
	if len(password) == 0 || len(password) > maxAdminPasswordBytes {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), password) == nil
}
