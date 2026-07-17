package auth

import "testing"

func TestPythonCompatibilityVector(t *testing.T) {
	const salt = "0123456789abcdef0123456789abcdef"
	const hash = "76fb0ab5903effdab16fe4509d3dfe16ed37b77f08be080b3195e3682b772af2"
	if !VerifyPassword("secret", hash, salt) {
		t.Fatal("failed to verify Python settings_store.py vector")
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	hash, salt, err := NewPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("secret", hash, salt) || VerifyPassword("wrong", hash, salt) {
		t.Fatal("password verification mismatch")
	}
}
