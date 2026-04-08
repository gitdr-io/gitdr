package crypto

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Optional client-side envelope encryption. Each stream gets a random data key (DEK)
// that encrypts the data in chunks with AES-256-GCM; the DEK is wrapped by a key
// encryption key (KEK) supplied via env (a KMS can wrap/unwrap the DEK later without
// changing the on-disk format). Chunked so multi-GB bundles never buffer in memory.
//
// Stream layout: magic | version | wrapNonce | wrappedDEK | streamPrefix | chunks…
// Each chunk is AES-256-GCM(DEK) over <= 64 KiB plaintext; the nonce is
// streamPrefix(4) || counter(7) || lastFlag(1), so truncation is detected.

const (
	encMagic   = "GDRE"
	encVersion = byte(1)
	encChunk   = 64 * 1024
	encDEK     = 32
	encNonce   = 12
	encTag     = 16
	encPrefix  = 4
)

const encHeaderLen = len(encMagic) + 1 + encNonce + (encDEK + encTag) + encPrefix

// EncryptionKeySize is the required KEK length (AES-256).
const EncryptionKeySize = 32

// ParseEncryptionKey accepts a 32-byte key as 64-char hex, base64, or raw bytes.
func ParseEncryptionKey(data []byte) ([]byte, error) {
	s := strings.TrimSpace(string(data))
	if len(s) == 64 {
		if k, err := hex.DecodeString(s); err == nil && len(k) == EncryptionKeySize {
			return k, nil
		}
	}
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == EncryptionKeySize {
		return k, nil
	}
	if len(data) == EncryptionKeySize {
		return data, nil
	}
	return nil, errors.New("encryption key must be 32 bytes (64-char hex, base64, or raw)")
}

// IsEncrypted reports whether b begins with the gitdr envelope magic, i.e. it is
// ciphertext produced by Encrypt/EncryptFile. Used to detect an encrypted artifact when
// no key was supplied, so restore can fail with a clear message.
func IsEncrypted(b []byte) bool {
	return len(b) >= len(encMagic) && string(b[:len(encMagic)]) == encMagic
}

// Encrypt streams src to dst as a gitdr envelope-encrypted stream.
func Encrypt(dst io.Writer, src io.Reader, kek []byte) error {
	if len(kek) != EncryptionKeySize {
		return errors.New("encryption key must be 32 bytes")
	}
	dek := make([]byte, encDEK)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	wrapped, wrapNonce, err := sealDEK(kek, dek)
	if err != nil {
		return err
	}
	prefix := make([]byte, encPrefix)
	if _, err := rand.Read(prefix); err != nil {
		return err
	}

	hdr := make([]byte, 0, encHeaderLen)
	hdr = append(hdr, encMagic...)
	hdr = append(hdr, encVersion)
	hdr = append(hdr, wrapNonce...)
	hdr = append(hdr, wrapped...)
	hdr = append(hdr, prefix...)
	if _, err := dst.Write(hdr); err != nil {
		return err
	}

	gcm, err := newGCM(dek)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(src, encChunk)
	buf := make([]byte, encChunk)
	for counter := uint64(0); ; counter++ {
		n, rerr := io.ReadFull(br, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return rerr
		}
		last := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if !last {
			if _, perr := br.Peek(1); perr == io.EOF {
				last = true
			}
		}
		if _, err := dst.Write(gcm.Seal(nil, chunkNonce(prefix, counter, last), buf[:n], nil)); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// Decrypt streams a gitdr envelope-encrypted src to dst.
func Decrypt(dst io.Writer, src io.Reader, kek []byte) error {
	if len(kek) != EncryptionKeySize {
		return errors.New("encryption key must be 32 bytes")
	}
	hdr := make([]byte, encHeaderLen)
	if _, err := io.ReadFull(src, hdr); err != nil {
		return fmt.Errorf("read encryption header: %w", err)
	}
	if string(hdr[:len(encMagic)]) != encMagic {
		return errors.New("not a gitdr-encrypted stream")
	}
	if hdr[len(encMagic)] != encVersion {
		return fmt.Errorf("unsupported encryption version %d", hdr[len(encMagic)])
	}
	off := len(encMagic) + 1
	wrapNonce := hdr[off : off+encNonce]
	wrapped := hdr[off+encNonce : off+encNonce+encDEK+encTag]
	prefix := hdr[off+encNonce+encDEK+encTag:]

	dek, err := openDEK(kek, wrapNonce, wrapped)
	if err != nil {
		return errors.New("decryption failed: wrong key or corrupt header")
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(src, encChunk+encTag)
	buf := make([]byte, encChunk+encTag)
	for counter := uint64(0); ; counter++ {
		n, rerr := io.ReadFull(br, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return rerr
		}
		last := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if !last {
			if _, perr := br.Peek(1); perr == io.EOF {
				last = true
			}
		}
		pt, oerr := gcm.Open(nil, chunkNonce(prefix, counter, last), buf[:n], nil)
		if oerr != nil {
			return errors.New("decryption failed: wrong key or corrupt data")
		}
		if _, err := dst.Write(pt); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// EncryptFile streams srcPath to a new encrypted dstPath.
func EncryptFile(srcPath, dstPath string, kek []byte) error {
	return fileXform(srcPath, dstPath, kek, Encrypt)
}

// DecryptFile streams an encrypted srcPath to a new plaintext dstPath.
func DecryptFile(srcPath, dstPath string, kek []byte) error {
	return fileXform(srcPath, dstPath, kek, Decrypt)
}

func fileXform(srcPath, dstPath string, kek []byte, fn func(io.Writer, io.Reader, []byte) error) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	if err := fn(out, in, kek); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func sealDEK(kek, dek []byte) (wrapped, nonce []byte, err error) {
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, encNonce)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return gcm.Seal(nil, nonce, dek, nil), nonce, nil
}

func openDEK(kek, nonce, wrapped []byte) ([]byte, error) {
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, wrapped, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(prefix []byte, counter uint64, last bool) []byte {
	n := make([]byte, encNonce)
	copy(n[:encPrefix], prefix)
	for i := 0; i < 7; i++ {
		n[10-i] = byte(counter >> (8 * i))
	}
	if last {
		n[11] = 1
	}
	return n
}
