package utils

import "testing"

func TestAesDecryptCBCRejectsInvalidCiphertext(t *testing.T) {
	key := []byte("abcd1234poiu5678bvbvnbnb")

	if _, err := AesDecryptCBC(nil, key); err == nil {
		t.Fatal("expected empty ciphertext error")
	}
	if _, err := AesDecryptCBC([]byte("short"), key); err == nil {
		t.Fatal("expected short ciphertext error")
	}
	if _, err := AesDecryptCBC(make([]byte, 16), key); err == nil {
		t.Fatal("expected invalid padding error")
	}
}

func TestAesEncryptDecryptCBCRoundTrip(t *testing.T) {
	key := "abcd1234poiu5678bvbvnbnb"
	want := `{"bridgePort":5678,"ip":"127.0.0.1","port":8080}`

	encrypted, err := AesEncryptCBCStr(want, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AesDecryptCBCStr(encrypted, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
