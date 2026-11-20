package mx.softpos.app.ui.onboarding

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.kyb.KybApi
import mx.softpos.app.kyb.KybApplication
import mx.softpos.app.kyb.KybDocumentReceipt
import mx.softpos.app.kyb.KybStatus
import mx.softpos.app.kyb.KybStatusValue
import mx.softpos.app.kyb.PickedDocument
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class FakeKybApi(var status: ApiResult<KybStatus> = ApiResult.Success(KybStatus(KybStatusValue.DRAFT, requiredDocuments = listOf("INE_FRONT", "CSF")), 200)) : KybApi {
    var saved: KybApplication? = null
    val uploaded = mutableListOf<String>()
    var uploadError: ApiResult.Error? = null
    var submitted = 0
    override fun getKyb(merchantId: String) = status
    override fun putKyb(merchantId: String, application: KybApplication): ApiResult<Unit> {
        saved = application
        return ApiResult.Success(Unit, 200)
    }
    override fun uploadDocument(merchantId: String, type: String, document: PickedDocument, onProgress: (Long, Long) -> Unit): ApiResult<KybDocumentReceipt> {
        uploadError?.let { uploadError = null; return it }
        val total = document.bytes.size.toLong()
        onProgress(total / 2, total)
        onProgress(total, total)
        uploaded += type
        return ApiResult.Success(KybDocumentReceipt("doc_$type", type), 201)
    }
    override fun submit(merchantId: String): ApiResult<Unit> {
        submitted++
        status = ApiResult.Success(KybStatus(KybStatusValue.SUBMITTED), 200)
        return ApiResult.Success(Unit, 200)
    }
}

@OptIn(ExperimentalCoroutinesApi::class)
class KybViewModelTest {
    private val dispatcher = UnconfinedTestDispatcher()
    private val api = FakeKybApi()

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    private fun vm(role: String = "owner") = KybViewModel(api, { "mer_1" }, { role }, dispatcher).also { it.load() }

    private val validForm = KybForm(
        legalName = "Juan Pérez", rfc = "pepj800101ab1", curp = "PEPJ800101HDFRRN09", taxRegime = "RESICO_PF", mcc = "5812",
        street = "Reforma", extNumber = "10", neighborhood = "Juárez", city = "Cuauhtémoc", state = "CDMX",
        postalCode = "06600", email = "juan@tacos.mx", phone = "55 1234 5678"
    )

    private fun doc() = PickedDocument(ByteArray(1_000) { 9 }, "image/jpeg", "x.jpg")

    @Test
    fun draftOwnerGoesThroughFormDocumentsAndSubmit() {
        val vm = vm()
        assertEquals(KybStep.FORM, vm.state.value.step)

        vm.onFormChange { validForm }
        assertEquals("PEPJ800101AB1", vm.state.value.form.rfc, "RFC is upper-cased while typing")
        vm.saveForm()
        assertEquals(KybStep.DOCUMENTS, vm.state.value.step)
        assertEquals("+525512345678", api.saved?.contactPhone)
        assertEquals(null, api.saved?.address?.intNumber)

        assertFalse(vm.state.value.canSubmit)
        val ine = doc()
        vm.upload("INE_FRONT", ine)
        assertEquals(UploadStatus.RECEIVED, vm.state.value.uploads["INE_FRONT"]?.status)
        assertTrue(ine.bytes.all { it == 0.toByte() }, "uploaded document bytes are zeroed")
        vm.submit()
        assertEquals(mx.softpos.app.ui.onboarding.KybError.INCOMPLETE, vm.state.value.error)
        assertEquals(0, api.submitted)

        vm.upload("CSF", doc())
        assertTrue(vm.state.value.canSubmit)
        vm.submit()
        assertEquals(KybStep.STATUS, vm.state.value.step)
        assertEquals(KybStatusValue.SUBMITTED, vm.state.value.status?.status)
    }

    @Test
    fun clientSideValidationBlocksBadRfcCurpAndContact() {
        val vm = vm()
        vm.onFormChange { validForm.copy(rfc = "XYZ", curp = "NOTACURP", postalCode = "123", email = "nope", phone = "123", mcc = "58") }
        vm.saveForm()
        val errors = vm.state.value.fieldErrors
        assertEquals(setOf(KybField.RFC, KybField.CURP, KybField.POSTAL_CODE, KybField.EMAIL, KybField.PHONE, KybField.MCC), errors)
        assertEquals(null, api.saved)
        assertEquals(KybStep.FORM, vm.state.value.step)
    }

    @Test
    fun failedUploadKeepsDocumentForRetryThenZeroes() {
        val vm = vm()
        vm.goToDocuments()
        api.uploadError = ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "")
        val ine = doc()
        vm.upload("INE_FRONT", ine)
        assertEquals(UploadStatus.FAILED, vm.state.value.uploads["INE_FRONT"]?.status)
        assertEquals(9.toByte(), ine.bytes[0], "kept for the retry")
        vm.retryUpload("INE_FRONT")
        assertEquals(UploadStatus.RECEIVED, vm.state.value.uploads["INE_FRONT"]?.status)
        assertEquals(0.toByte(), ine.bytes[0])

        val huge = PickedDocument(ByteArray(KybViewModel.MAX_DOCUMENT_BYTES + 1), "application/pdf", "csf.pdf")
        vm.upload("CSF", huge)
        assertEquals(mx.softpos.app.ui.onboarding.KybError.TOO_LARGE, vm.state.value.error)
    }

    @Test
    fun rejectedShowsStatusWithReasonAndOwnerCanEdit() {
        api.status = ApiResult.Success(KybStatus(KybStatusValue.REJECTED, reason = "CSF vencida"), 200)
        val vm = vm()
        assertEquals(KybStep.STATUS, vm.state.value.step)
        assertEquals("CSF vencida", vm.state.value.status?.reason)
        vm.edit()
        assertEquals(KybStep.FORM, vm.state.value.step)
    }

    @Test
    fun cashierOnlySeesStatus() {
        val vm = vm(role = "cashier")
        assertEquals(KybStep.STATUS, vm.state.value.step)
        vm.edit()
        assertEquals(KybStep.STATUS, vm.state.value.step)
        vm.saveForm()
        assertEquals(mx.softpos.app.ui.onboarding.KybError.NOT_OWNER, vm.state.value.error)
    }

    @Test
    fun noApplicationYetStartsADraftButMissingEndpointDoesNot() {
        api.status = ApiResult.Error(404, "KYB_NOT_FOUND", "")
        assertEquals(KybStep.FORM, vm().state.value.step)
        api.status = ApiResult.Error(404, "HTTP_404", "")
        val missing = vm()
        assertEquals(KybStep.STATUS, missing.state.value.step)
        assertEquals(mx.softpos.app.ui.onboarding.KybError.NOT_AVAILABLE, missing.state.value.error)
    }

    @Test
    fun taxRegimeUsesTheBackendEnumNotTheSatNumericKey() {
        // pkg/kyb knownRegimes: the PUT is rejected (400 INVALID_KYB_DATA tax_regime) for "626".
        assertEquals(listOf("RESICO_PF", "RESICO_PM", "ACTIVIDAD_EMPRESARIAL_PF", "PLATAFORMAS_TECNOLOGICAS_PF", "GENERAL_PM", "OTHER"), KybValidators.TAX_REGIMES)
        assertFalse(KybValidators.isValidTaxRegime("626"))
        assertTrue(KybValidators.isValidTaxRegime("RESICO_PF"))
        assertEquals("RESICO_PF", KybForm().taxRegime, "the only regime the MVP supports is preselected")
        val vm = vm()
        vm.onFormChange { validForm }
        vm.saveForm()
        assertEquals("RESICO_PF", api.saved?.taxRegime)
    }

    @Test
    fun validatorsAndImageSizing() {
        assertTrue(KybValidators.isValidRfc("TAC010101AB1"))
        assertTrue(KybValidators.isValidRfc("PEPJ800101AB1"))
        assertFalse(KybValidators.isValidRfc("PEPJ8001AB1"))
        assertTrue(KybValidators.isValidCurp("PEPJ800101HDFRRN09"))
        assertFalse(KybValidators.isValidCurp("PEPJ801301HDFRRN09")) // month 13
        assertTrue(KybValidators.isValidEmail("a@b.mx"))
        assertFalse(KybValidators.isValidPostalCode("6600"))
        assertEquals(2048 to 1536, DocumentImageSizing.targetSize(4032, 3024))
        assertEquals(1152 to 2048, DocumentImageSizing.targetSize(2268, 4032))
        assertEquals(800 to 600, DocumentImageSizing.targetSize(800, 600))
    }
}
