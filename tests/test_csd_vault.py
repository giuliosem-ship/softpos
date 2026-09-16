import unittest
import secrets
from internal.identity.csd_vault import (
    CSDVaultService,
    EncryptedCSD,
    DecryptedCSD,
    CSDVaultError
)

class TestCSDVault(unittest.TestCase):
    def setUp(self):
        # 32 bytes (256-bit) Master Key in hex (simulating AWS/GCP KMS master key)
        self.master_key = secrets.token_hex(32)
        self.vault = CSDVaultService(master_key_hex=self.master_key)

    def test_store_and_decrypt_csd(self):
        merchant_id = "merch_oxxo_01"
        dummy_cer = b"-----BEGIN CERTIFICATE-----\nMIIC...DUMMY_CSD_CER\n-----END CERTIFICATE-----"
        dummy_key = b"-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIF...DUMMY_CSD_KEY\n-----END ENCRYPTED PRIVATE KEY-----"
        password = "SuperSecretCSDPassword123!"

        # 1. Store and encrypt
        encrypted: EncryptedCSD = self.vault.store_csd(
            merchant_id=merchant_id,
            cer_bytes=dummy_cer,
            key_bytes=dummy_key,
            password=password
        )

        self.assertEqual(encrypted.merchant_id, merchant_id)
        # Ensure raw secrets are not present in plain text in the encrypted object
        self.assertNotIn(dummy_cer, encrypted.encrypted_cer)
        self.assertNotIn(dummy_key, encrypted.encrypted_key)
        self.assertNotIn(password.encode(), encrypted.encrypted_password)

        # 2. Decrypt in memory for signing
        decrypted: DecryptedCSD = self.vault.decrypt_csd_for_signing(encrypted)

        self.assertEqual(decrypted.merchant_id, merchant_id)
        self.assertEqual(decrypted.cer_bytes, dummy_cer)
        self.assertEqual(decrypted.key_bytes, dummy_key)
        self.assertEqual(decrypted.password, password)

    def test_decrypt_with_wrong_master_key_fails(self):
        merchant_id = "merch_oxxo_02"
        encrypted = self.vault.store_csd(
            merchant_id=merchant_id,
            cer_bytes=b"dummy_cer",
            key_bytes=b"dummy_key",
            password="pwd"
        )

        # Attacker tries to decrypt with a different master key
        attacker_vault = CSDVaultService(master_key_hex=secrets.token_hex(32))
        with self.assertRaises(CSDVaultError):
            attacker_vault.decrypt_csd_for_signing(encrypted)

    def test_tampered_payload_fails(self):
        merchant_id = "merch_oxxo_03"
        encrypted = self.vault.store_csd(
            merchant_id=merchant_id,
            cer_bytes=b"dummy_cer",
            key_bytes=b"dummy_key",
            password="pwd"
        )

        # Tamper with encrypted key bytes
        tampered_bytes = bytearray(encrypted.encrypted_key)
        tampered_bytes[0] ^= 0xFF
        
        tampered = EncryptedCSD(
            merchant_id=encrypted.merchant_id,
            encrypted_cer=encrypted.encrypted_cer,
            encrypted_key=bytes(tampered_bytes),
            encrypted_password=encrypted.encrypted_password,
            encrypted_dek=encrypted.encrypted_dek,
            iv=encrypted.iv,
            auth_tag=encrypted.auth_tag
        )

        with self.assertRaises(CSDVaultError):
            self.vault.decrypt_csd_for_signing(tampered)

    def test_empty_csd_inputs_raise(self):
        with self.assertRaises((ValueError, CSDVaultError)):
            self.vault.store_csd(
                merchant_id="m1",
                cer_bytes=b"",
                key_bytes=b"key",
                password="pwd"
            )

if __name__ == "__main__":
    unittest.main()
