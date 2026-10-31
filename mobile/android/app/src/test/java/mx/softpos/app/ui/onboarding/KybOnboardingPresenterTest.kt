package mx.softpos.app.ui.onboarding

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class KybOnboardingPresenterTest {

    @Test
    fun `validateCsdBundle returns Valid for valid Persona Fisica and Persona Moral bundles`() {
        val pfBundle = CsdUploadBundle(
            merchantId = "m_pf_01",
            rfc = " garm920115abc ",
            cerFileName = "sello_sat.CER",
            cerBytes = byteArrayOf(0x30, 0x82.toByte(), 0x01, 0x0A),
            keyFileName = "llave_privada.key",
            keyBytes = byteArrayOf(0x30, 0x81.toByte(), 0xFF.toByte()),
            password = "secretPassword123"
        )
        val resultPf = KybOnboardingPresenter.validateCsdBundle(pfBundle)
        assertTrue(resultPf is KybValidationResult.Valid)
        val validPf = resultPf as KybValidationResult.Valid
        assertEquals("m_pf_01", validPf.merchantId)
        assertEquals("GARM920115ABC", validPf.normalizedRfc)
        assertEquals(4, validPf.cerSizeBytes)
        assertEquals(3, validPf.keySizeBytes)

        val pmBundle = pfBundle.copy(merchantId = "m_pm_02", rfc = "EMP210510XY9")
        val resultPm = KybOnboardingPresenter.validateCsdBundle(pmBundle)
        assertTrue(resultPm is KybValidationResult.Valid)
        assertEquals("EMP210510XY9", (resultPm as KybValidationResult.Valid).normalizedRfc)
    }

    @Test
    fun `validateCsdBundle collects all validation errors in exact order`() {
        val invalidBundle = CsdUploadBundle(
            merchantId = "   ",
            rfc = "INVALID_RFC",
            cerFileName = "certificate.pem",
            cerBytes = byteArrayOf(),
            keyFileName = "private.txt",
            keyBytes = byteArrayOf(),
            password = "123"
        )
        val result = KybOnboardingPresenter.validateCsdBundle(invalidBundle)
        assertTrue(result is KybValidationResult.Invalid)
        val invalid = result as KybValidationResult.Invalid
        assertEquals(
            listOf(
                "MERCHANT_ID_REQUIRED",
                "INVALID_RFC_FORMAT",
                "INVALID_CER_FILE",
                "INVALID_KEY_FILE",
                "CSD_PASSWORD_TOO_SHORT"
            ),
            invalid.errors
        )
    }

    @Test
    fun `validateCsdBundle rejects empty cer or key bytes even with valid extensions`() {
        val emptyFilesBundle = CsdUploadBundle(
            merchantId = "m_01",
            rfc = "GARM920115ABC",
            cerFileName = "csd.cer",
            cerBytes = ByteArray(0),
            keyFileName = "csd.key",
            keyBytes = ByteArray(0),
            password = "validPass"
        )
        val result = KybOnboardingPresenter.validateCsdBundle(emptyFilesBundle)
        assertTrue(result is KybValidationResult.Invalid)
        assertEquals(
            listOf("INVALID_CER_FILE", "INVALID_KEY_FILE"),
            (result as KybValidationResult.Invalid).errors
        )
    }
}
