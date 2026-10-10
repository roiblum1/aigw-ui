package secretbox

import (
	"bytes"
	"testing"
)

func TestSealAndOpen(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("apiVersion: v1\nkind: Config")
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Error("the sealed value contains the plaintext")
	}
	opened, err := box.Open(sealed)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Errorf("opened %q, %v", opened, err)
	}

	// The same value sealed twice differs, so equal secrets cannot be told
	// from the database.
	again, _ := box.Seal(plain)
	if bytes.Equal(sealed, again) {
		t.Error("sealing the same value twice gave the same bytes")
	}
}

func TestOpenRefusesWhatItDidNotSeal(t *testing.T) {
	box, _ := New(bytes.Repeat([]byte{7}, 32))
	sealed, _ := box.Seal([]byte("secret"))

	changed := append([]byte(nil), sealed...)
	changed[len(changed)-1] ^= 1
	if _, err := box.Open(changed); err == nil {
		t.Error("opened a value that was changed after sealing")
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open(sealed); err == nil {
		t.Error("opened a value with another key")
	}
	if _, err := box.Open([]byte("short")); err == nil {
		t.Error("opened a value shorter than a nonce")
	}
}

func TestNewRefusesAKeyOfTheWrongLength(t *testing.T) {
	if _, err := New([]byte("too short")); err == nil {
		t.Error("accepted a 9-byte key")
	}
}
