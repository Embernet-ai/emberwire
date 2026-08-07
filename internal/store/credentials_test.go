package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSecret = "a-secret-that-would-be-in-a-k8s-secret"

func TestCredentialRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")

	c := NewCredentialStore(path, testSecret)
	c.Set("node1", map[string]string{"user": "svc-press", "password": "hunter2"})
	c.Set("node2", map[string]string{"token": "abc123"})
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Nothing readable on disk.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	for _, secret := range []string{"hunter2", "svc-press", "abc123"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("credential file contains the plaintext %q", secret)
		}
	}

	c2 := NewCredentialStore(path, testSecret)
	if err := c2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c2.Get("node1")
	if got["password"] != "hunter2" || got["user"] != "svc-press" {
		t.Errorf("node1 credentials = %#v", got)
	}
	if c2.Get("node2")["token"] != "abc123" {
		t.Errorf("node2 credentials = %#v", c2.Get("node2"))
	}
	if c2.MigratedFromLegacy() {
		t.Error("a file we wrote was reported as legacy")
	}
}

func TestCredentialWrongSecretIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	c := NewCredentialStore(path, testSecret)
	c.Set("n", map[string]string{"password": "hunter2"})
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	wrong := NewCredentialStore(path, "not-the-right-secret")
	err := wrong.Load()
	if !errors.Is(err, ErrBadSecret) {
		t.Errorf("Load with the wrong secret = %v, want ErrBadSecret", err)
	}

	none := NewCredentialStore(path, "")
	if err := none.Load(); !errors.Is(err, ErrNoSecret) {
		t.Errorf("Load with no secret = %v, want ErrNoSecret", err)
	}
}

func TestCredentialTamperingIsDetected(t *testing.T) {
	// The reason for moving off Node-RED's AES-256-CTR. CTR has no MAC, so
	// anyone who can write the file can flip chosen plaintext bits. GCM
	// authenticates, so a modified file fails to open at all.
	path := filepath.Join(t.TempDir(), "credentials.json")
	c := NewCredentialStore(path, testSecret)
	c.Set("n", map[string]string{"password": "hunter2"})
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var f credFile
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing envelope: %v", err)
	}

	ct, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		t.Fatalf("decoding ciphertext: %v", err)
	}
	ct[0] ^= 0x01 // flip one bit
	f.Data = base64.StdEncoding.EncodeToString(ct)

	tampered, _ := json.Marshal(f)
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("writing tampered file: %v", err)
	}

	c2 := NewCredentialStore(path, testSecret)
	if err := c2.Load(); !errors.Is(err, ErrBadSecret) {
		t.Errorf("Load of a tampered file = %v, want ErrBadSecret", err)
	}
}

func TestCredentialFormatDowngradeIsRejected(t *testing.T) {
	// The format string is authenticated as additional data, so rewriting it
	// cannot be used to steer decryption at a weaker scheme.
	path := filepath.Join(t.TempDir(), "credentials.json")
	c := NewCredentialStore(path, testSecret)
	c.Set("n", map[string]string{"password": "hunter2"})
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var f credFile
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &f)
	f.Format = "something-else-v1"
	out, _ := json.Marshal(f)
	_ = os.WriteFile(path, out, 0o600)

	c2 := NewCredentialStore(path, testSecret)
	// An unrecognised format falls through to the plaintext path, which cannot
	// parse a ciphertext envelope as a credential map.
	if err := c2.Load(); err == nil {
		t.Error("Load accepted a file with a rewritten format field")
	}
}

// writeLegacyNodeREDCredentials produces a file in Node-RED's exact format:
// AES-256-CTR, key = raw SHA-256 of the secret, hex IV prepended to base64
// ciphertext, all under a single "$" key.
func writeLegacyNodeREDCredentials(t *testing.T, path, secret string, creds map[string]map[string]string) {
	t.Helper()

	plain, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	ct := make([]byte, len(plain))
	cipher.NewCTR(block, iv).XORKeyStream(ct, plain)

	envelope := map[string]string{
		"$": hex.EncodeToString(iv) + base64.StdEncoding.EncodeToString(ct),
	}
	out, _ := json.Marshal(envelope)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("writing legacy file: %v", err)
	}
}

func TestImportsNodeREDLegacyCredentials(t *testing.T) {
	// Refusing to read an existing deployment's credentials would make
	// migration impossible. This is the compatibility path.
	path := filepath.Join(t.TempDir(), "flows_cred.json")
	writeLegacyNodeREDCredentials(t, path, testSecret, map[string]map[string]string{
		"broker1": {"user": "plant", "password": "correct-horse"},
	})

	c := NewCredentialStore(path, testSecret)
	if err := c.Load(); err != nil {
		t.Fatalf("Load of a Node-RED credential file: %v", err)
	}
	got := c.Get("broker1")
	if got["password"] != "correct-horse" || got["user"] != "plant" {
		t.Errorf("imported credentials = %#v", got)
	}
	if !c.MigratedFromLegacy() {
		t.Error("MigratedFromLegacy() = false; the operator would not be told a migration happened")
	}
}

func TestLegacyCredentialsAreReEncryptedOnSave(t *testing.T) {
	// Read the old format, write the new one. Nothing is ever written back as
	// AES-256-CTR.
	path := filepath.Join(t.TempDir(), "flows_cred.json")
	writeLegacyNodeREDCredentials(t, path, testSecret, map[string]map[string]string{
		"broker1": {"password": "correct-horse"},
	})

	c := NewCredentialStore(path, testSecret)
	if err := c.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if c.MigratedFromLegacy() {
		t.Error("still reported as legacy after a save")
	}

	raw, _ := os.ReadFile(path)
	var f credFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("re-encrypted file does not parse as the new envelope: %v", err)
	}
	if f.Format != credFormatGCM {
		t.Errorf("format after migration = %q, want %q", f.Format, credFormatGCM)
	}
	if f.Salt == "" || f.Nonce == "" {
		t.Error("migrated file is missing its salt or nonce")
	}

	c2 := NewCredentialStore(path, testSecret)
	if err := c2.Load(); err != nil {
		t.Fatalf("reloading the migrated file: %v", err)
	}
	if c2.Get("broker1")["password"] != "correct-horse" {
		t.Error("credential did not survive migration")
	}
}

func TestLegacyWrongSecretDetectedByPlausibilityCheck(t *testing.T) {
	// CTR has no tag, so a wrong key yields plausible-looking garbage rather
	// than an error. Checking that the result is valid JSON is the only
	// integrity signal available — which is exactly why the write path moved to
	// GCM.
	path := filepath.Join(t.TempDir(), "flows_cred.json")
	writeLegacyNodeREDCredentials(t, path, testSecret, map[string]map[string]string{
		"b": {"password": "x"},
	})
	c := NewCredentialStore(path, "wrong-secret")
	if err := c.Load(); !errors.Is(err, ErrBadSecret) {
		t.Errorf("Load = %v, want ErrBadSecret", err)
	}
}

func TestCredentialMergeKeepsUnchangedValues(t *testing.T) {
	// The editor sends a sentinel for a password field it displayed but did not
	// touch. Treating it as a new value would overwrite every password with the
	// literal string the first time anyone opened an edit dialog.
	c := NewCredentialStore(filepath.Join(t.TempDir(), "c.json"), testSecret)
	c.Set("n", map[string]string{"password": "original", "user": "admin"})

	c.Merge("n", map[string]any{"password": unchangedSentinel, "user": "operator"})

	got := c.Get("n")
	if got["password"] != "original" {
		t.Errorf("password = %q, want it left alone", got["password"])
	}
	if got["user"] != "operator" {
		t.Errorf("user = %q, want operator", got["user"])
	}

	// An empty string is an explicit clear.
	c.Merge("n", map[string]any{"password": ""})
	if _, still := c.Get("n")["password"]; still {
		t.Error("an empty value did not clear the credential")
	}

	// A non-string is stringified rather than dropped; some node types persist
	// numeric ports as credentials.
	c.Merge("n", map[string]any{"port": 1883.0})
	if got := c.Get("n")["port"]; got != "1883" {
		t.Errorf("numeric credential = %q, want \"1883\"", got)
	}
}

func TestCredentialPrune(t *testing.T) {
	// A credential must not outlive the node it belonged to, sitting encrypted
	// on the PVC forever.
	c := NewCredentialStore(filepath.Join(t.TempDir(), "c.json"), testSecret)
	c.Set("live", map[string]string{"p": "1"})
	c.Set("deleted", map[string]string{"p": "2"})

	if removed := c.Prune(map[string]bool{"live": true}); removed != 1 {
		t.Errorf("Prune removed %d, want 1", removed)
	}
	if c.Get("deleted") != nil {
		t.Error("a deleted node's credentials survived Prune")
	}
	if c.Get("live") == nil {
		t.Error("Prune removed a live node's credentials")
	}
}

func TestCredentialGetReturnsACopy(t *testing.T) {
	c := NewCredentialStore(filepath.Join(t.TempDir(), "c.json"), testSecret)
	c.Set("n", map[string]string{"password": "original"})

	got := c.Get("n")
	got["password"] = "mutated"

	if c.Get("n")["password"] != "original" {
		t.Error("Get returned the internal map; a caller mutated the store")
	}
	if c.Get("missing") != nil {
		t.Error("Get on an unknown node returned non-nil")
	}
}

func TestCredentialEmptySetRemoves(t *testing.T) {
	c := NewCredentialStore(filepath.Join(t.TempDir(), "c.json"), testSecret)
	c.Set("n", map[string]string{"p": "1"})
	c.Set("n", nil)
	if c.Get("n") != nil {
		t.Error("Set with an empty map did not remove the entry")
	}
	if len(c.NodeIDs()) != 0 {
		t.Errorf("NodeIDs() = %v, want empty", c.NodeIDs())
	}
}

func TestCredentialLoadMissingFileIsNotAnError(t *testing.T) {
	c := NewCredentialStore(filepath.Join(t.TempDir(), "nope.json"), testSecret)
	if err := c.Load(); err != nil {
		t.Errorf("Load of a missing file = %v, want nil", err)
	}
}

func TestCredentialSaltAndNonceAreFreshPerSave(t *testing.T) {
	// Reusing a nonce with the same key destroys GCM's guarantees outright.
	path := filepath.Join(t.TempDir(), "c.json")
	c := NewCredentialStore(path, testSecret)
	c.Set("n", map[string]string{"p": "same-value-every-time"})

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		if err := c.Save(); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
		raw, _ := os.ReadFile(path)
		var f credFile
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("parsing: %v", err)
		}
		key := f.Salt + "|" + f.Nonce
		if seen[key] {
			t.Fatalf("salt/nonce pair repeated on save %d", i)
		}
		seen[key] = true
	}
}

func TestSecretsEqual(t *testing.T) {
	if !SecretsEqual("abc", "abc") {
		t.Error("SecretsEqual said equal secrets differ")
	}
	if SecretsEqual("abc", "abd") || SecretsEqual("abc", "abcd") {
		t.Error("SecretsEqual said different secrets match")
	}
}

func BenchmarkCredentialSave(b *testing.B) {
	// Argon2id is deliberately expensive. This measures how expensive, so the
	// cost of a deploy is a known quantity rather than a surprise.
	path := filepath.Join(b.TempDir(), "c.json")
	c := NewCredentialStore(path, testSecret)
	c.Set("n", map[string]string{"password": "hunter2"})
	b.ReportAllocs()
	for b.Loop() {
		if err := c.Save(); err != nil {
			b.Fatal(err)
		}
	}
}
