// Copyright 2020 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vfile

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

type signedFile struct {
	signers []*openpgp.Entity
	content string
}

func (s signedFile) write(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write([]byte(s.content)); err != nil {
		return err
	}

	sigf, err := os.OpenFile(fmt.Sprintf("%s.sig", path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer sigf.Close()
	for _, signer := range s.signers {
		if err := openpgp.DetachSign(sigf, signer, strings.NewReader(s.content), nil); err != nil {
			return err
		}
	}
	return nil
}

type normalFile struct {
	content string
}

func (n normalFile) write(path string) error {
	return os.WriteFile(path, []byte(n.content), 0o600)
}

func writeHashedFile(path, content string) ([]byte, error) {
	c := []byte(content)
	if err := os.WriteFile(path, c, 0o600); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(c)
	return hash[:], nil
}

func TestOpenSignedFile(t *testing.T) {
	keyFiles := []string{"key0", "key1"}

	// EntityGenerate generates the entities in testdata/. The entities are
	// cached because they take 40+ seconds to generate in arm64 QEMU.
	t.Run("EntityGenerate", func(t *testing.T) {
		t.Skip("uncomment this to generate the entities")

		if err := os.MkdirAll("testdata", 0o777); err != nil {
			t.Fatal(err)
		}

		for i, k := range keyFiles {
			// You would think this Config would be sufficient to
			// generate the same each time for the test, but it is
			// not the case (and I don't know why).
			var s int64
			conf := &packet.Config{
				Rand: rand.New(rand.NewSource(int64(i))),
				Time: func() time.Time {
					s++
					return time.Unix(s, 0)
				},
			}
			key, err := openpgp.NewEntity("goog", "goog", "goog@goog", conf)
			if err != nil {
				t.Fatal(err)
			}

			f, err := os.Create(filepath.Join("testdata", k))
			if err != nil {
				t.Fatal(err)
			}
			if err := key.SerializePrivate(f, conf); err != nil {
				f.Close()
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})

	// This depends on the keys generated by EntityGenerate.
	var keys []*openpgp.Entity
	for _, k := range keyFiles {
		b, err := os.ReadFile(filepath.Join("testdata", k))
		if err != nil {
			t.Fatal(err)
		}
		key, err := openpgp.ReadEntity(packet.NewReader(bytes.NewBuffer(b)))
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}

	ring := openpgp.EntityList{keys[0]}

	dir := t.TempDir()

	signed := signedFile{
		signers: openpgp.EntityList{keys[0]},
		content: "foo",
	}
	signedPath := filepath.Join(dir, "signed_by_key0")
	if err := signed.write(signedPath); err != nil {
		t.Fatal(err)
	}

	signed2 := signedFile{
		signers: openpgp.EntityList{keys[1]},
		content: "foo",
	}
	signed2Path := filepath.Join(dir, "signed_by_key1")
	if err := signed2.write(signed2Path); err != nil {
		t.Fatal(err)
	}

	signed12 := signedFile{
		signers: openpgp.EntityList{keys[0], keys[1]},
		content: "foo",
	}
	signed12Path := filepath.Join(dir, "signed_by_both.sig")
	if err := signed12.write(signed12Path); err != nil {
		t.Fatal(err)
	}

	normalPath := filepath.Join(dir, "unsigned")
	if err := os.WriteFile(normalPath, []byte("foo"), 0o777); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		desc             string
		path             string
		keyring          openpgp.KeyRing
		want             error
		isSignatureValid bool
	}{
		{
			desc:             "signed file",
			keyring:          ring,
			path:             signedPath,
			want:             nil,
			isSignatureValid: true,
		},
		{
			desc:             "signed file w/ two signatures (key0 ring)",
			keyring:          ring,
			path:             signed12Path,
			want:             nil,
			isSignatureValid: true,
		},
		{
			desc:             "signed file w/ two signatures (key1 ring)",
			keyring:          openpgp.EntityList{keys[1]},
			path:             signed12Path,
			want:             nil,
			isSignatureValid: true,
		},
		{
			desc:    "nil keyring",
			keyring: nil,
			path:    signed2Path,
			want: ErrUnsigned{
				Path: signed2Path,
				Err:  ErrNoKeyRing,
			},
			isSignatureValid: false,
		},
		{
			desc:    "non-nil empty keyring",
			keyring: openpgp.EntityList{},
			path:    signed2Path,
			want: ErrUnsigned{
				Path: signed2Path,
				Err:  errors.ErrUnknownIssuer,
			},
			isSignatureValid: false,
		},
		{
			desc:    "signed file does not match keyring",
			keyring: openpgp.EntityList{keys[1]},
			path:    signedPath,
			want: ErrUnsigned{
				Path: signedPath,
				Err:  errors.ErrUnknownIssuer,
			},
			isSignatureValid: false,
		},
		{
			desc:    "unsigned file",
			keyring: ring,
			path:    normalPath,
			want: ErrUnsigned{
				Path: normalPath,
				Err: &os.PathError{
					Op:   "open",
					Path: fmt.Sprintf("%s.sig", normalPath),
					Err:  syscall.ENOENT,
				},
			},
			isSignatureValid: false,
		},
		{
			desc:    "file does not exist",
			keyring: ring,
			path:    filepath.Join(dir, "foo"),
			want: &os.PathError{
				Op:   "open",
				Path: filepath.Join(dir, "foo"),
				Err:  syscall.ENOENT,
			},
			isSignatureValid: false,
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			f, gotErr := OpenSignedSigFile(tt.keyring, tt.path)
			if !reflect.DeepEqual(gotErr, tt.want) {
				t.Errorf("openSignedFile(%v, %q) = %v, want %v", tt.keyring, tt.path, gotErr, tt.want)
			}

			if isSignatureValid := (gotErr == nil); isSignatureValid != tt.isSignatureValid {
				t.Errorf("isSignatureValid(%v) = %v, want %v", gotErr, isSignatureValid, tt.isSignatureValid)
			}

			// Make sure that the file is readable from position 0.
			if f != nil {
				content, err := io.ReadAll(f)
				if err != nil {
					t.Errorf("Could not read content: %v", err)
				}
				if got := string(content); got != "foo" {
					t.Errorf("ReadAll = %v, want \"foo\"", got)
				}
			}
		})
	}
}

func TestReadSignedImage(t *testing.T) {
	for _, tt := range []struct {
		desc       string
		path       string
		wantKeyCnt int
		wantError  bool
	}{
		{
			desc:       "Correct read key0",
			path:       "testdata/key0",
			wantError:  false,
			wantKeyCnt: 2,
		},
		{
			desc:       "Correct read key1",
			path:       "testdata/key1",
			wantError:  false,
			wantKeyCnt: 2,
		},
		{
			desc:       "Read nonRSA key",
			path:       "testdata/dsakey",
			wantError:  true,
			wantKeyCnt: 0,
		},
		{
			desc:       "Multikey ring",
			path:       "testdata/keyring0+1+dsa",
			wantError:  false,
			wantKeyCnt: 4,
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			ring, err := GetKeyRing(tt.path)
			if err != nil {
				t.Fatalf("GetKeyRing(%s) Failed with err: %v", tt.path, err)
			}
			gotKeys, gotErr := GetRSAKeysFromRing(ring)
			if (gotErr == nil) == tt.wantError {
				t.Errorf("GetRSAKeysFromRing(%s) = %v, want %v", tt.path, gotErr, tt.wantError)
			}
			var gotCnt int
			if gotKeys == nil {
				gotCnt = 0
			} else {
				gotCnt = len(gotKeys)
			}

			if tt.wantKeyCnt != gotCnt {
				t.Errorf("GetRSAKeysFromRing(%s) returned %d keys, want %d", tt.path, gotCnt, tt.wantKeyCnt)
			}
		})
	}
}

func TestOpenHashedFile(t *testing.T) {
	dir := t.TempDir()

	hashedPath := filepath.Join(dir, "hashed")
	hash, err := writeHashedFile(hashedPath, "foo")
	if err != nil {
		t.Fatal(err)
	}

	emptyPath := filepath.Join(dir, "empty")
	emptyHash, err := writeHashedFile(emptyPath, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		desc        string
		path        string
		hash        []byte
		want        error
		isHashValid bool
		wantContent string
	}{
		{
			desc:        "correct hash",
			path:        hashedPath,
			hash:        hash,
			want:        nil,
			isHashValid: true,
			wantContent: "foo",
		},
		{
			desc: "wrong hash",
			path: hashedPath,
			hash: []byte{0x99, 0x77},
			want: ErrInvalidHash{
				Path: hashedPath,
				Err: ErrHashMismatch{
					Got:  hash,
					Want: []byte{0x99, 0x77},
				},
			},
			isHashValid: false,
			wantContent: "foo",
		},
		{
			desc: "no hash",
			path: hashedPath,
			hash: []byte{},
			want: ErrInvalidHash{
				Path: hashedPath,
				Err:  ErrNoExpectedHash,
			},
			isHashValid: false,
			wantContent: "foo",
		},
		{
			desc:        "empty file",
			path:        emptyPath,
			hash:        emptyHash,
			want:        nil,
			isHashValid: true,
			wantContent: "",
		},
		{
			desc: "nonexistent file",
			path: filepath.Join(dir, "doesnotexist"),
			hash: nil,
			want: &os.PathError{
				Op:   "open",
				Path: filepath.Join(dir, "doesnotexist"),
				Err:  syscall.ENOENT,
			},
			isHashValid: false,
		},
	} {
		t.Run(tt.desc, func(t *testing.T) {
			f, err := OpenHashedFile256(tt.path, tt.hash)
			if !reflect.DeepEqual(err, tt.want) {
				t.Errorf("OpenHashedFile256(%s, %x) = %v, want %v", tt.path, tt.hash, err, tt.want)
			}

			if isHashValid := (err == nil); isHashValid != tt.isHashValid {
				t.Errorf("isHashValid(%v) = %v, want %v", err, isHashValid, tt.isHashValid)
			}

			// Make sure that the file is readable from position 0.
			if f != nil {
				content, err := io.ReadAll(f)
				if err != nil {
					t.Errorf("Could not read content: %v", err)
				}
				if got := string(content); got != tt.wantContent {
					t.Errorf("ReadAll = %v, want %s", got, tt.wantContent)
				}
			}
		})
	}
}
