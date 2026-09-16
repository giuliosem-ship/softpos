from dataclasses import dataclass, field
from typing import Any
import os
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.exceptions import InvalidTag

@dataclass(frozen=True)
class EncryptedCSD:
    merchant_id: str
    encrypted_cer: bytes
    encrypted_key: bytes
    encrypted_password: bytes
    encrypted_dek: bytes
    iv: bytes
    auth_tag: bytes = b""

@dataclass(frozen=True)
class DecryptedCSD:
    merchant_id: str
    cer_bytes: bytes
    key_bytes: bytes
    password: str

class CSDVaultError(Exception):
    pass

class CSDVaultService:
    def __init__(self, master_key_hex: str):
        self.master_key = bytes.fromhex(master_key_hex)

    def store_csd(self, merchant_id: str, cer_bytes: bytes, key_bytes: bytes, password: str) -> EncryptedCSD:
        if not merchant_id or not cer_bytes or not key_bytes or not password:
            raise ValueError("All inputs must be non-empty")

        dek = os.urandom(32)
        nonce = os.urandom(12)

        aesgcm_dek = AESGCM(dek)
        encrypted_cer = aesgcm_dek.encrypt(nonce, cer_bytes, None)
        encrypted_key = aesgcm_dek.encrypt(nonce, key_bytes, None)
        encrypted_password = aesgcm_dek.encrypt(nonce, password.encode('utf-8'), None)

        aesgcm_master = AESGCM(self.master_key)
        encrypted_dek = aesgcm_master.encrypt(nonce, dek, None)

        return EncryptedCSD(
            merchant_id=merchant_id,
            encrypted_cer=encrypted_cer,
            encrypted_key=encrypted_key,
            encrypted_password=encrypted_password,
            encrypted_dek=encrypted_dek,
            iv=nonce
        )

    def decrypt_csd_for_signing(self, encrypted_csd: EncryptedCSD) -> DecryptedCSD:
        try:
            aesgcm_master = AESGCM(self.master_key)
            dek = aesgcm_master.decrypt(encrypted_csd.iv, encrypted_csd.encrypted_dek, None)

            aesgcm_dek = AESGCM(dek)
            cer_bytes = aesgcm_dek.decrypt(encrypted_csd.iv, encrypted_csd.encrypted_cer, None)
            key_bytes = aesgcm_dek.decrypt(encrypted_csd.iv, encrypted_csd.encrypted_key, None)
            password = aesgcm_dek.decrypt(encrypted_csd.iv, encrypted_csd.encrypted_password, None).decode('utf-8')

            return DecryptedCSD(
                merchant_id=encrypted_csd.merchant_id,
                cer_bytes=cer_bytes,
                key_bytes=key_bytes,
                password=password
            )
        except (InvalidTag, Exception) as e:
            raise CSDVaultError("Decryption failed") from e
