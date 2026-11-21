package mx.softpos.app.ui.fiscal

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.fiscal.CsdApi
import mx.softpos.app.fiscal.CsdFile
import mx.softpos.app.fiscal.CsdInfo
import mx.softpos.app.fiscal.CsdIssue
import mx.softpos.app.fiscal.TestCertificates
import mx.softpos.app.merchant.MerchantProfile
import mx.softpos.app.ui.charge.FakeMerchantApi
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.time.Instant

class FakeCsdApi(
    var current: ApiResult<CsdInfo> = ApiResult.Error(404, "CSD_NOT_FOUND", """{"error":"CSD_NOT_FOUND"}"""),
    var uploadResult: ApiResult<CsdInfo>? = null
) : CsdApi {
    var uploads = 0
    /** Copies of what was sent, taken before the ViewModel zeroes the originals. */
    var sentPassword: String? = null
    var sentKey: ByteArray? = null
    var sentKeyRef: ByteArray? = null
    var sentPasswordRef: CharArray? = null

    override fun getCsd(merchantId: String) = current

    override fun uploadCsd(merchantId: String, cer: CsdFile, key: CsdFile, password: CharArray): ApiResult<CsdInfo> {
        uploads++
        sentPassword = String(password)
        sentKey = key.bytes.copyOf()
        sentKeyRef = key.bytes
        sentPasswordRef = password
        val r = uploadResult ?: ApiResult.Success(CsdInfo("30001000000500003416", "TEST010203AB1", "2025-01-01T00:00:00Z", "2029-01-01T00:00:00Z", "ACTIVE"), 200)
        if (r is ApiResult.Success) current = r
        return r
    }
}

@OptIn(ExperimentalCoroutinesApi::class)
class CsdViewModelTest {
    private val dispatcher = UnconfinedTestDispatcher()
    private val api = FakeCsdApi()
    private val merchant = FakeMerchantApi(
        profile = ApiResult.Success(MerchantProfile("mer_1", "TEST010203AB1", "Comercio", true, ""), 200)
    )
    private var now = Instant.parse("2026-06-01T00:00:00Z")
    private val cer = TestCertificates.csd()
    private fun key() = ByteArray(1200).also { it[0] = 0x30; it[1] = 7 }

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    private fun vm(role: String = "owner") =
        CsdViewModel(api, merchant, { "mer_1" }, { role }, dispatcher) { now }.also { it.load() }

    @Test
    fun notConfiguredThenUploadShowsStoredCertificate() {
        val vm = vm()
        assertTrue(vm.state.value.notConfigured)
        assertEquals("TEST010203AB1", vm.state.value.merchantRfc)
        vm.onCerPicked("csd.cer", cer.copyOf())
        val key = key()
        vm.onKeyPicked("csd.key", key)
        assertTrue(vm.state.value.canUpload)
        assertEquals("30001000000500003416", vm.state.value.cerSummary?.serial)

        val password = "s3creta".toCharArray()
        vm.upload(password)
        assertEquals("s3creta", api.sentPassword)
        assertEquals(7.toByte(), api.sentKey!![1])
        assertTrue(password.all { it == '\u0000' }, "password zeroed")
        assertTrue(key.all { it == 0.toByte() }, "key bytes zeroed after the attempt")

        val s = vm.state.value
        assertTrue(s.uploaded)
        assertFalse(s.notConfigured)
        assertEquals("ACTIVE", s.csd?.status)
        assertNull(s.cerName)
        assertFalse(s.canUpload)
    }

    @Test
    fun rejectedUploadShowsReasonAndRequiresKeyAgain() {
        api.uploadResult = ApiResult.Error(422, "CSD_INVALID", """{"error":"CSD_INVALID","reason":"KEY_PASSWORD_INVALID"}""")
        val vm = vm()
        vm.onCerPicked("csd.cer", cer.copyOf())
        vm.onKeyPicked("csd.key", key())
        val password = "mala".toCharArray()
        vm.upload(password)
        val s = vm.state.value
        assertEquals(CsdUploadError.INVALID, s.uploadError)
        assertEquals("KEY_PASSWORD_INVALID", s.invalidReason)
        assertTrue(password.all { it == '\u0000' })
        assertTrue(api.sentKeyRef!!.all { it == 0.toByte() })
        assertNull(s.keyName)
        assertFalse(s.canUpload, "the key must be picked again")
    }

    @Test
    fun preChecksBlockUpload() {
        val vm = vm()
        merchant.profile = ApiResult.Success(MerchantProfile("mer_1", "XAXX010101000"), 200)
        vm.load()
        vm.onCerPicked("csd.cer", cer.copyOf())
        vm.onKeyPicked("csd.key", "not a key".toByteArray())
        assertEquals(listOf(CsdIssue.RFC_MISMATCH, CsdIssue.KEY_INVALID), vm.state.value.issues)
        val password = "x".toCharArray()
        vm.upload(password)
        assertEquals(0, api.uploads)
        assertTrue(password.all { it == '\u0000' }, "zeroed even when nothing is sent")

        vm.onCerPicked("otro.cer", null)
        assertTrue(CsdIssue.CER_UNREADABLE in vm.state.value.issues)
    }

    @Test
    fun expiryWarningUnderThirtyDays() {
        api.current = ApiResult.Success(CsdInfo("1", "TEST010203AB1", "2025-01-01T00:00:00Z", "2026-06-20T00:00:00-06:00", "ACTIVE"), 200)
        val vm = vm()
        assertEquals(19L, vm.state.value.daysLeft)
        assertTrue(vm.state.value.expiringSoon)
        now = Instant.parse("2026-06-20T08:00:00Z")
        vm.load()
        assertTrue(vm.state.value.expired)
        assertFalse(vm.state.value.expiringSoon)
    }

    @Test
    fun cashierCannotUpload() {
        val vm = vm(role = "cashier")
        vm.onCerPicked("csd.cer", cer.copyOf())
        vm.onKeyPicked("csd.key", key())
        assertFalse(vm.state.value.canUpload)
        vm.upload("x".toCharArray())
        assertEquals(0, api.uploads)
    }
}
