package utils

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
)

// =================== CBC ======================
func AesEncryptCBCStr(orig string, key string) (txt string, err error) {
	d, err := AesEncryptCBC([]byte(orig), []byte(key))
	if err != nil {
		return "", err
	}
	txt = base64.StdEncoding.EncodeToString(d)
	return
}
func AesEncryptCBC(origData []byte, key []byte) (encrypted []byte, err error) {
	// 分组秘钥
	// NewCipher该函数限制了输入k的长度必须为16, 24或者32
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	blockSize := block.BlockSize()                              // 获取秘钥块的长度
	origData = pkcs5Padding(origData, blockSize)                // 补全码
	blockMode := cipher.NewCBCEncrypter(block, key[:blockSize]) // 加密模式
	encrypted = make([]byte, len(origData))                     // 创建数组
	blockMode.CryptBlocks(encrypted, origData)                  // 加密
	return encrypted, nil
}

func AesDecryptCBCStr(msg, key string) (decrypted []byte, err error) {
	data, err := base64.StdEncoding.DecodeString(msg)
	if err != nil {
		return
	}
	return AesDecryptCBC(data, []byte(key))
}

func AesDecryptCBC(encrypted []byte, key []byte) (decrypted []byte, err error) {
	block, err := aes.NewCipher(key) // 分组秘钥
	if err != nil {
		return nil, err
	}
	blockSize := block.BlockSize() // 获取秘钥块的长度
	if len(encrypted) == 0 || len(encrypted)%blockSize != 0 {
		return nil, fmt.Errorf("invalid ciphertext length %d", len(encrypted))
	}
	blockMode := cipher.NewCBCDecrypter(block, key[:blockSize]) // 加密模式
	decrypted = make([]byte, len(encrypted))                    // 创建数组
	blockMode.CryptBlocks(decrypted, encrypted)                 // 解密
	decrypted, err = pkcs5UnPadding(decrypted)                  // 去除补全码
	if err != nil {
		return nil, err
	}
	return decrypted, nil
}
func pkcs5Padding(ciphertext []byte, blockSize int) []byte {
	padding := blockSize - len(ciphertext)%blockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(ciphertext, padtext...)
}
func pkcs5UnPadding(origData []byte) ([]byte, error) {
	length := len(origData)
	if length == 0 {
		return nil, errors.New("empty plaintext")
	}
	unpadding := int(origData[length-1])
	if unpadding == 0 || unpadding > length {
		return nil, errors.New("invalid padding")
	}
	for _, b := range origData[length-unpadding:] {
		if int(b) != unpadding {
			return nil, errors.New("invalid padding")
		}
	}
	return origData[:(length - unpadding)], nil
}
