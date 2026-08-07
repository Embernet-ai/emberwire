package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Credential storage.
//
// Node-RED encrypts flows_cred.json with AES-256-CTR, keying it with a raw
// SHA-256 of the user's credentialSecret. Two problems with that construction,
// neither of which has a CVE but both of which are real:
//
//   - CTR provides no integrity. There is no MAC, so ciphertext is malleable:
//     anyone who can write the credential file can flip chosen bits of the
//     plaintext deterministically. On a shared PVC that is a live path from
//     "can write a file" to "can change a broker password to one they know".
//   - Plain SHA-256 is not a key derivation function. It is fast by design,
//     which is the opposite of what you want standing between a stolen file and
//     a weak passphrase.
//
// Emberwire writes AES-256-GCM keyed with Argon2id. It still reads the legacy
// format, because refusing to import an existing Node-RED deployment's
// credentials would make migration impossible — but anything read that way is
// re-encrypted under the new scheme on the next save.

const (
	// credFormatGCM is the current on-disk format.
	credFormatGCM = "emberwire-aes256gcm-argon2id-v1"

	// Argon2id parameters. 64 MiB and one pass over four lanes is the
	// RFC 9106 second recommended option, chosen because this runs on edge
	// hardware where a 2 GiB parameter set would not fit alongside the flows.
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// ErrNoSecret is returned when an encrypted credential file exists but no
// secret was supplied to decrypt it.
var ErrNoSecret = errors.New("credentials are encrypted but no credential secret was provided")

// ErrBadSecret is returned when decryption fails authentication, which means
// either the wrong secret or a tampered file. The two are deliberately not
// distinguished — telling an attacker which one they got is a free oracle.
var ErrBadSecret = errors.New("could not decrypt credentials: wrong secret or the file has been tampered with")

// credFile is the on-disk envelope.
type credFile struct {
	Format string `json:"format"`
	Salt   string `json:"salt"`  // base64, Argon2id salt
	Nonce  string `json:"nonce"` // base64, GCM nonce
	Data   string `json:"data"`  // base64, ciphertext + GCM tag
}

// CredentialStore holds per-node secrets, encrypted at rest.
type CredentialStore struct {
	path   string
	secret []byte

	mu    sync.RWMutex
	creds map[string]map[string]string

	// legacy records that the file was read in Node-RED's format, so the next
	// save can migrate it and the API can tell the operator it happened.
	legacy bool
}

// NewCredentialStore returns a store backed by path. An empty secret means
// credentials are held in memory only and never written — appropriate for
// tests, and refused at startup in production by the config layer.
func NewCredentialStore(path, secret string) *CredentialStore {
	cs := &CredentialStore{path: path, creds: map[string]map[string]string{}}
	if secret != "" {
		cs.secret = []byte(secret)
	}
	return cs
}

// HasSecret reports whether a secret is configured.
func (c *CredentialStore) HasSecret() bool { return len(c.secret) > 0 }

// MigratedFromLegacy reports whether the file was read in Node-RED's format.
func (c *CredentialStore) MigratedFromLegacy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.legacy
}

// Get returns the decrypted credentials for a node.
func (c *CredentialStore) Get(nodeID string) map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	src := c.creds[nodeID]
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Set replaces the credentials for a node. An empty map removes the entry.
func (c *CredentialStore) Set(nodeID string, creds map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(creds) == 0 {
		delete(c.creds, nodeID)
		return
	}
	cp := make(map[string]string, len(creds))
	for k, v := range creds {
		cp[k] = v
	}
	c.creds[nodeID] = cp
}

// Merge applies an incoming credential set from a deploy.
//
// The editor only sends values the user actually changed, and sends the
// sentinel "__PWRD__" for a field it is displaying but did not touch. Treating
// that sentinel as a new value would overwrite every password with the literal
// string the first time anyone opened an edit dialog.
const unchangedSentinel = "__PWRD__"

func (c *CredentialStore) Merge(nodeID string, incoming map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cur := c.creds[nodeID]
	if cur == nil {
		cur = map[string]string{}
	}
	for k, v := range incoming {
		s, ok := v.(string)
		if !ok {
			// A non-string credential is stringified rather than dropped; some
			// node types persist numeric ports as credentials.
			s = fmt.Sprint(v)
		}
		switch s {
		case unchangedSentinel:
			// Keep whatever is already stored.
		case "":
			delete(cur, k)
		default:
			cur[k] = s
		}
	}
	if len(cur) == 0 {
		delete(c.creds, nodeID)
		return
	}
	c.creds[nodeID] = cur
}

// Prune removes credentials belonging to nodes that no longer exist. Without
// it, a credential outlives the node it belonged to and sits encrypted on the
// PVC indefinitely.
func (c *CredentialStore) Prune(liveNodes map[string]bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for id := range c.creds {
		if !liveNodes[id] {
			delete(c.creds, id)
			removed++
		}
	}
	return removed
}

// NodeIDs returns the ids that have credentials, for diagnostics. It never
// returns the values.
func (c *CredentialStore) NodeIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.creds))
	for id := range c.creds {
		out = append(out, id)
	}
	return out
}

// Load reads and decrypts the credential file. A missing file is not an error.
func (c *CredentialStore) Load() error {
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", c.path, err)
	}
	if len(data) == 0 {
		return nil
	}

	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("parsing %s: %w", c.path, err)
	}

	// Node-RED's format is a single "$" key holding hex IV + base64 ciphertext.
	if legacy, ok := envelope["$"].(string); ok {
		plain, err := c.decryptLegacy(legacy)
		if err != nil {
			return err
		}
		if err := c.ingest(plain); err != nil {
			return err
		}
		c.mu.Lock()
		c.legacy = true
		c.mu.Unlock()
		return nil
	}

	if format, _ := envelope["format"].(string); format == credFormatGCM {
		var f credFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("parsing %s: %w", c.path, err)
		}
		plain, err := c.decryptGCM(f)
		if err != nil {
			return err
		}
		return c.ingest(plain)
	}

	// An unencrypted file. Node-RED writes one when no credentialSecret is set,
	// and so do we — but it is worth being explicit that this is what happened.
	return c.ingest(data)
}

func (c *CredentialStore) ingest(plain []byte) error {
	var raw map[string]map[string]string
	if err := json.Unmarshal(plain, &raw); err != nil {
		return fmt.Errorf("parsing decrypted credentials: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if raw == nil {
		raw = map[string]map[string]string{}
	}
	c.creds = raw
	return nil
}

// Save encrypts and writes the credential file.
func (c *CredentialStore) Save() error {
	c.mu.RLock()
	plain, err := json.Marshal(c.creds)
	c.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("serialising credentials: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(c.path), err)
	}

	var out []byte
	if !c.HasSecret() {
		out = plain
	} else {
		env, err := c.encryptGCM(plain)
		if err != nil {
			return err
		}
		out, err = json.MarshalIndent(env, "", "  ")
		if err != nil {
			return fmt.Errorf("serialising credential envelope: %w", err)
		}
	}

	// 0600 without exception: this file is the whole point of the exercise.
	if err := WriteFileAtomic(c.path, out, 0o600); err != nil {
		return err
	}

	c.mu.Lock()
	c.legacy = false
	c.mu.Unlock()
	return nil
}

func (c *CredentialStore) encryptGCM(plain []byte) (credFile, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return credFile{}, fmt.Errorf("generating salt: %w", err)
	}
	key := argon2.IDKey(c.secret, salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	block, err := aes.NewCipher(key)
	if err != nil {
		return credFile{}, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return credFile{}, fmt.Errorf("creating GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return credFile{}, fmt.Errorf("generating nonce: %w", err)
	}

	// The format string is authenticated as additional data, so an attacker
	// cannot downgrade the file by rewriting the format field.
	ct := gcm.Seal(nil, nonce, plain, []byte(credFormatGCM))

	return credFile{
		Format: credFormatGCM,
		Salt:   base64.StdEncoding.EncodeToString(salt),
		Nonce:  base64.StdEncoding.EncodeToString(nonce),
		Data:   base64.StdEncoding.EncodeToString(ct),
	}, nil
}

func (c *CredentialStore) decryptGCM(f credFile) ([]byte, error) {
	if !c.HasSecret() {
		return nil, ErrNoSecret
	}
	salt, err := base64.StdEncoding.DecodeString(f.Salt)
	if err != nil {
		return nil, fmt.Errorf("decoding salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(f.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decoding nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		return nil, fmt.Errorf("decoding ciphertext: %w", err)
	}

	key := argon2.IDKey(c.secret, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, ErrBadSecret
	}
	plain, err := gcm.Open(nil, nonce, ct, []byte(credFormatGCM))
	if err != nil {
		// Authentication failure. Do not distinguish a wrong key from a
		// tampered file — that distinction is a free oracle for an attacker.
		return nil, ErrBadSecret
	}
	return plain, nil
}

// decryptLegacy reads Node-RED's AES-256-CTR format: a hex-encoded 16-byte IV
// concatenated with base64 ciphertext, keyed with a raw SHA-256 of the secret.
//
// Read-only on purpose. Importing an existing deployment has to work, but
// nothing is ever written back in this format.
func (c *CredentialStore) decryptLegacy(payload string) ([]byte, error) {
	if !c.HasSecret() {
		return nil, ErrNoSecret
	}
	const ivHexLen = 32 // 16 bytes, hex encoded
	if len(payload) < ivHexLen {
		return nil, fmt.Errorf("legacy credential payload is too short to contain an IV")
	}

	iv, err := hex.DecodeString(payload[:ivHexLen])
	if err != nil {
		return nil, fmt.Errorf("decoding legacy IV: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(payload[ivHexLen:])
	if err != nil {
		return nil, fmt.Errorf("decoding legacy ciphertext: %w", err)
	}

	key := sha256.Sum256(c.secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("creating legacy cipher: %w", err)
	}
	plain := make([]byte, len(ct))
	cipher.NewCTR(block, iv).XORKeyStream(plain, ct)

	// CTR has no authentication tag, so a wrong key yields plausible-looking
	// garbage rather than an error. Validating that the result is the JSON
	// object we expect is the only integrity check available here — which is
	// precisely the weakness that motivated moving to GCM.
	if !json.Valid(plain) {
		return nil, ErrBadSecret
	}
	return plain, nil
}

// SecretsEqual compares two secrets in constant time. Used when rotating.
func SecretsEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
