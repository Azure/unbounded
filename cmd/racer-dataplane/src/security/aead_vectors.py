#!/usr/bin/env python3
"""Print independent page-AEAD vectors using installed libsodium; writes no files."""
import ctypes
import ctypes.util
import hashlib
import struct


def main():
    sodium = ctypes.CDLL(ctypes.util.find_library("sodium"))
    sodium.sodium_version_string.restype = ctypes.c_char_p
    assert sodium.sodium_init() >= 0
    encrypt = sodium.crypto_aead_xchacha20poly1305_ietf_encrypt
    encrypt.argtypes = [
        ctypes.c_void_p, ctypes.POINTER(ctypes.c_ulonglong), ctypes.c_void_p,
        ctypes.c_ulonglong, ctypes.c_void_p, ctypes.c_ulonglong, ctypes.c_void_p,
        ctypes.c_void_p, ctypes.c_void_p,
    ]
    encrypt.restype = ctypes.c_int
    print("// libsodium", sodium.sodium_version_string().decode())
    cache = b"33333333-3333-4333-8333-333333333333"
    nonce = bytes([2]) * 24
    key = bytes([7]) * 32
    for length in [1, 15, 16, 17, 63, 64, 65, 255, 256, 257, 16777215, 16777216]:
        plaintext = (bytes((i * 31 + 7) % 256 for i in range(256))
                     * ((length + 255) // 256))[:length]
        aad = (b"racer/page/aead/v1\0" + struct.pack(">I", len(cache)) + cache
               + bytes([3]) * 32 + struct.pack(">I", 4) + b'"v1"'
               + struct.pack(">Q", 7) + bytes([1]) * 16 + nonce
               + struct.pack(">II", length, length + 16))
        output = ctypes.create_string_buffer(length + 16)
        output_length = ctypes.c_ulonglong()
        assert encrypt(output, ctypes.byref(output_length), plaintext, length,
                       aad, len(aad), None, nonce, key) == 0
        assert output_length.value == length + 16
        print(f'({length}, "{hashlib.sha256(output.raw).hexdigest()}"),')


if __name__ == "__main__":
    main()
