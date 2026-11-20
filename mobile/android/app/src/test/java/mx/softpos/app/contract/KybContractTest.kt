package mx.softpos.app.contract

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.kyb.HttpKybApi
import mx.softpos.app.kyb.KybDocumentType
import mx.softpos.app.kyb.KybStatusValue
import mx.softpos.app.kyb.PickedDocument
import mx.softpos.app.kyb.invalidKybFields
import mx.softpos.app.kyb.kybChecksFailed
import mx.softpos.app.ui.onboarding.KybError
import mx.softpos.app.ui.onboarding.KybField
import mx.softpos.app.ui.onboarding.KybStep
import mx.softpos.app.ui.onboarding.KybViewModel
import mx.softpos.app.ui.onboarding.UploadStatus
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

/** KYB client and onboarding flow against the backend's real response bodies (pkg/api/handlers_kyb.go). */
@OptIn(ExperimentalCoroutinesApi::class)
class KybContractTest {
    private lateinit var server: MockWebServer
    private val dispatcher = UnconfinedTestDispatcher()

    @BeforeEach fun setUp() {
        server = MockWebServer().apply { start() }
        Dispatchers.setMain(dispatcher)
    }

    @AfterEach fun tearDown() {
        server.shutdown()
        Dispatchers.resetMain()
    }

    private fun api() = HttpKybApi(server.url("/").toString(), OkHttpClient())
    private fun respond(code: Int, fixture: String) = server.enqueue(MockResponse().setResponseCode(code).setBody(Fixtures.read(fixture)))
    private fun vm(role: String = "owner") = KybViewModel(api(), { "m_fix" }, { role }, dispatcher)

    @Test
    fun getDecodesEveryBackendField() {
        respond(200, "kyb_get_owner_partial_docs.json")
        val k = (api().getKyb("m_fix") as ApiResult.Success).data
        assertEquals(KybStatusValue.DRAFT, k.status)
        assertEquals("DOCUMENTS_INCOMPLETE", k.reason)
        assertEquals(KybDocumentType.ALL, k.requiredDocuments, "backend always lists the six documents")
        assertEquals(listOf(KybDocumentType.INE_FRONT), k.uploadedDocuments)
        assertEquals("RESICO_PF", k.application?.taxRegime)
        assertEquals("+525512345678", k.application?.contactPhone)

        respond(200, "kyb_get_cashier.json")
        assertNull((api().getKyb("m_fix") as ApiResult.Success).data.application, "application is owner-only")

        respond(200, "kyb_get_draft_empty.json")
        assertEquals("", (api().getKyb("m_fix") as ApiResult.Success).data.application?.rfc)
    }

    @Test
    fun ownerResumesWithTheSavedApplicationAndUploadedDocuments() {
        respond(200, "kyb_get_owner_partial_docs.json")
        val vm = vm().also { it.load() }
        val s = vm.state.value
        assertEquals(KybStep.FORM, s.step)
        assertEquals("CACX7605101P8", s.form.rfc)
        assertEquals("06000", s.form.postalCode)
        assertEquals("5512345678", s.form.phone)
        assertEquals(UploadStatus.RECEIVED, s.uploads[KybDocumentType.INE_FRONT]?.status)
        assertNull(s.uploads[KybDocumentType.CSF])
        assertFalse(s.canSubmit)
    }

    @Test
    fun putValidationErrorsMarkTheBackendFields() {
        respond(200, "kyb_get_owner_partial_docs.json")
        val vm = vm().also { it.load() }
        respond(400, "kyb_put_invalid_400.json")
        vm.saveForm()
        val s = vm.state.value
        assertEquals(KybStep.FORM, s.step)
        assertEquals(setOf(KybField.RFC, KybField.TAX_REGIME), s.fieldErrors)
        assertEquals(KybError.INVALID_DATA, s.error)
    }

    @Test
    fun invalidFieldsAndChecksFailedHelpers() {
        val invalid = ApiResult.Error(400, "INVALID_KYB_DATA", Fixtures.read("kyb_put_invalid_400.json"))
        assertEquals(listOf("rfc", "tax_regime"), invalid.invalidKybFields())
        val failed = ApiResult.Error(422, "KYB_CHECKS_FAILED", Fixtures.read("kyb_submit_checks_failed_422.json"))
        assertEquals("DOCUMENTS_INCOMPLETE", failed.kybChecksFailed()?.reason)
        assertNull(invalid.kybChecksFailed())
    }

    @Test
    fun submitChecksFailedShowsTheReasonAndStaysEditable() {
        respond(200, "kyb_get_owner_partial_docs.json")
        val vm = vm().also { it.load() }
        // Pretend the remaining documents are there so the client lets the owner submit.
        KybDocumentType.ALL.drop(1).forEach { type ->
            respond(201, "kyb_upload_201.json")
            vm.upload(type, PickedDocument(ByteArray(10) { 1 }, "image/jpeg", "x.jpg"))
        }
        assertTrue(vm.state.value.canSubmit)
        respond(422, "kyb_submit_checks_failed_422.json")
        vm.submit()
        val s = vm.state.value
        assertEquals(KybStep.STATUS, s.step)
        assertEquals(KybStatusValue.DRAFT, s.status?.status)
        assertEquals("DOCUMENTS_INCOMPLETE", s.status?.reason)
        assertEquals(KybError.CHECKS_FAILED, s.error)
    }

    @Test
    fun submitOkUsesTheReturnedStatusInReview() {
        respond(200, "kyb_get_owner_partial_docs.json")
        val vm = vm().also { it.load() }
        KybDocumentType.ALL.drop(1).forEach { type ->
            respond(201, "kyb_upload_201.json")
            vm.upload(type, PickedDocument(ByteArray(10) { 1 }, "image/jpeg", "x.jpg"))
        }
        respond(200, "kyb_submit_ok_200.json")
        vm.submit()
        assertEquals(KybStep.STATUS, vm.state.value.step)
        assertEquals(KybStatusValue.IN_REVIEW, vm.state.value.status?.status)
        assertEquals(1 + 5 + 1, server.requestCount, "no extra GET after submit")
    }

    @Test
    fun alreadySubmittedReloadsTheStatus() {
        respond(200, "kyb_get_owner_partial_docs.json")
        val vm = vm().also { it.load() }
        KybDocumentType.ALL.drop(1).forEach { type ->
            respond(201, "kyb_upload_201.json")
            vm.upload(type, PickedDocument(ByteArray(10) { 1 }, "image/jpeg", "x.jpg"))
        }
        respond(409, "kyb_resubmit_409.json")
        respond(200, "kyb_submit_ok_200.json")
        vm.submit()
        assertEquals(KybStep.STATUS, vm.state.value.step)
        assertEquals(KybStatusValue.IN_REVIEW, vm.state.value.status?.status)
    }

    @Test
    fun uploadReceiptIs201() {
        respond(201, "kyb_upload_201.json")
        val r = api().uploadDocument("m_fix", "INE_FRONT", PickedDocument(ByteArray(5), "image/jpeg", "a.jpg")) { _, _ -> }
        assertNotNull((r as ApiResult.Success).data.documentId)
        assertEquals(201, r.statusCode)
    }

    @Test
    fun clientDocumentCapMatchesTheBackendLimit() {
        assertEquals(5 * 1024 * 1024, KybViewModel.MAX_DOCUMENT_BYTES, "pkg/kyb MaxDocumentBytes = 5 MiB")
    }
}
