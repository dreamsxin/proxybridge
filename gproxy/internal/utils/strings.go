package utils

import (
	"math/rand/v2"
	"strings"
)

var characters = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func RandStringByLen(size int) string {
	randomString := strings.Builder{}

	for i := 0; i < size; i++ {
		index := rand.IntN(len(characters))
		randomString.WriteByte(characters[index])
	}
	return randomString.String()
}
