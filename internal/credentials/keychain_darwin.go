//go:build darwin && cgo

package credentials

/*
#cgo LDFLAGS: -framework Security -framework Foundation -framework LocalAuthentication
#include <stdlib.h>
void *turnbeam_auth_context(void);
void turnbeam_auth_cancel(void *context);
void turnbeam_auth_release(void *context);
int turnbeam_keychain(void *context, int operation, const unsigned char *key, long key_len, unsigned char **out, long *out_len);
void turnbeam_secret_free(unsigned char *key, long key_len);
*/
import "C"

import (
	"context"
	"fmt"
	"unsafe"
)

// Keychain addresses only this CLI's non-synchronizing generic-password item.
// Native calls avoid exposing keys in process arguments or subprocess output.
type Keychain struct{}

// KeychainSupported is false in builds without the native macOS adapter.
const KeychainSupported = true

// Operation codes understood by turnbeam_keychain in keychain_darwin.m.
const (
	opLoad = iota
	opHas
	opSave
	opDelete
)

const errSecItemNotFound = -25300

func (Keychain) Load(ctx context.Context) ([]byte, error) { return keychainCall(ctx, opLoad, nil) }
func (Keychain) Has(ctx context.Context) (bool, error) {
	_, err := keychainCall(ctx, opHas, nil)
	if err == ErrNotFound {
		return false, nil
	}
	return err == nil, err
}
func (Keychain) Save(ctx context.Context, key []byte) error {
	if !valid(key) {
		return ErrInvalid
	}
	_, err := keychainCall(ctx, opSave, key)
	return err
}
func (Keychain) Delete(ctx context.Context) error {
	_, err := keychainCall(ctx, opDelete, nil)
	return err
}

func keychainCall(ctx context.Context, operation int, key []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	auth := C.turnbeam_auth_context()
	if auth == nil {
		return nil, fmt.Errorf("Keychain authentication context unavailable")
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		C.turnbeam_auth_cancel(auth)
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
		C.turnbeam_auth_release(auth)
	}()
	var input *C.uchar
	if len(key) > 0 {
		input = (*C.uchar)(unsafe.Pointer(&key[0]))
	}
	var output *C.uchar
	var length C.long
	status := C.turnbeam_keychain(auth, C.int(operation), input, C.long(len(key)), &output, &length)
	if output != nil {
		defer C.turnbeam_secret_free(output, length)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if status == errSecItemNotFound {
		return nil, ErrNotFound
	}
	if status != 0 {
		return nil, fmt.Errorf("Keychain operation failed (OSStatus %d); unlock or allow access in Keychain Access", int(status))
	}
	if operation != opLoad {
		return nil, nil
	}
	if output == nil || length < 1 || length > maxKeyBytes {
		return nil, ErrInvalid
	}
	return C.GoBytes(unsafe.Pointer(output), C.int(length)), nil
}
