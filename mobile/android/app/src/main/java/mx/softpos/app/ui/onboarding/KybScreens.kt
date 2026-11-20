package mx.softpos.app.ui.onboarding

import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.FilterChip
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import mx.softpos.app.R
import mx.softpos.app.kyb.KybDocumentType
import mx.softpos.app.kyb.KybStatusValue

@Composable
fun KybScreen(viewModel: KybViewModel, reader: DocumentReader, onDone: () -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }
    Column(
        Modifier.fillMaxSize().padding(16.dp).verticalScroll(rememberScrollState()),
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        Text(stringResource(R.string.kyb_title), style = MaterialTheme.typography.headlineSmall)
        when (state.step) {
            KybStep.LOADING -> CircularProgressIndicator()
            KybStep.FORM -> KybFormView(state, viewModel)
            KybStep.DOCUMENTS -> KybDocumentsView(state, viewModel, reader)
            KybStep.STATUS -> KybStatusView(state, viewModel)
        }
        state.error?.let { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error) }
        TextButton(onClick = onDone) { Text(stringResource(R.string.action_back)) }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun KybFormView(state: KybUiState, vm: KybViewModel) {
    val f = state.form
    val errors = state.fieldErrors
    Text(stringResource(R.string.kyb_form_intro))
    KybTextField(R.string.kyb_legal_name, f.legalName, KybField.LEGAL_NAME in errors) { v -> vm.onFormChange { it.copy(legalName = v) } }
    KybTextField(R.string.kyb_rfc, f.rfc, KybField.RFC in errors, errorText = R.string.kyb_rfc_error) { v -> vm.onFormChange { it.copy(rfc = v) } }
    KybTextField(R.string.kyb_curp, f.curp, KybField.CURP in errors, errorText = R.string.kyb_curp_error) { v -> vm.onFormChange { it.copy(curp = v) } }
    Text(stringResource(R.string.kyb_tax_regime), style = MaterialTheme.typography.titleSmall)
    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        KybValidators.TAX_REGIMES.forEach { regime ->
            FilterChip(
                selected = f.taxRegime == regime,
                onClick = { vm.onFormChange { it.copy(taxRegime = regime) } },
                label = { Text(stringResource(taxRegimeLabel(regime))) }
            )
        }
    }
    if (KybField.TAX_REGIME in errors) Text(stringResource(R.string.kyb_tax_regime_error), color = MaterialTheme.colorScheme.error)
    KybTextField(R.string.kyb_mcc, f.mcc, KybField.MCC in errors, errorText = R.string.kyb_mcc_error, keyboard = KeyboardType.Number) { v ->
        vm.onFormChange { it.copy(mcc = v.filter(Char::isDigit).take(4)) }
    }
    Text(stringResource(R.string.kyb_address), style = MaterialTheme.typography.titleMedium)
    KybTextField(R.string.kyb_street, f.street, KybField.STREET in errors) { v -> vm.onFormChange { it.copy(street = v) } }
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        KybTextField(R.string.kyb_ext_number, f.extNumber, KybField.EXT_NUMBER in errors, modifier = Modifier.weight(1f)) { v ->
            vm.onFormChange { it.copy(extNumber = v) }
        }
        KybTextField(R.string.kyb_int_number, f.intNumber, false, modifier = Modifier.weight(1f)) { v -> vm.onFormChange { it.copy(intNumber = v) } }
    }
    KybTextField(R.string.kyb_neighborhood, f.neighborhood, KybField.NEIGHBORHOOD in errors) { v -> vm.onFormChange { it.copy(neighborhood = v) } }
    KybTextField(R.string.kyb_city, f.city, KybField.CITY in errors) { v -> vm.onFormChange { it.copy(city = v) } }
    KybTextField(R.string.kyb_state, f.state, KybField.STATE in errors) { v -> vm.onFormChange { it.copy(state = v) } }
    KybTextField(R.string.kyb_postal_code, f.postalCode, KybField.POSTAL_CODE in errors, errorText = R.string.kyb_postal_code_error, keyboard = KeyboardType.Number) { v ->
        vm.onFormChange { it.copy(postalCode = v.filter(Char::isDigit).take(5)) }
    }
    Text(stringResource(R.string.kyb_contact), style = MaterialTheme.typography.titleMedium)
    KybTextField(R.string.kyb_email, f.email, KybField.EMAIL in errors, errorText = R.string.kyb_email_error, keyboard = KeyboardType.Email) { v ->
        vm.onFormChange { it.copy(email = v) }
    }
    KybTextField(R.string.kyb_phone, f.phone, KybField.PHONE in errors, errorText = R.string.login_error_invalid_phone, keyboard = KeyboardType.Phone) { v ->
        vm.onFormChange { it.copy(phone = v) }
    }
    Button(onClick = vm::saveForm, enabled = !state.isBusy, modifier = Modifier.fillMaxWidth()) {
        Text(stringResource(R.string.kyb_save_continue))
    }
    if (state.isBusy) CircularProgressIndicator()
}

private fun taxRegimeLabel(regime: String): Int = when (regime) {
    KybValidators.RESICO_PF -> R.string.kyb_regime_resico_pf
    "RESICO_PM" -> R.string.kyb_regime_resico_pm
    "ACTIVIDAD_EMPRESARIAL_PF" -> R.string.kyb_regime_actividad_empresarial
    "PLATAFORMAS_TECNOLOGICAS_PF" -> R.string.kyb_regime_plataformas
    "GENERAL_PM" -> R.string.kyb_regime_general_pm
    else -> R.string.kyb_regime_other
}

@Composable
private fun KybTextField(
    label: Int,
    value: String,
    isError: Boolean,
    modifier: Modifier = Modifier,
    errorText: Int = R.string.kyb_required,
    keyboard: KeyboardType = KeyboardType.Text,
    onChange: (String) -> Unit
) {
    OutlinedTextField(
        value = value,
        onValueChange = onChange,
        label = { Text(stringResource(label)) },
        isError = isError,
        supportingText = if (isError) ({ Text(stringResource(errorText)) }) else null,
        keyboardOptions = KeyboardOptions(keyboardType = keyboard),
        singleLine = true,
        modifier = modifier.fillMaxWidth()
    )
}

@Composable
private fun KybDocumentsView(state: KybUiState, vm: KybViewModel, reader: DocumentReader) {
    val scope = rememberCoroutineScope()
    var targetType by rememberSaveable { mutableStateOf<String?>(null) }
    var unreadable by remember { mutableStateOf(false) }

    fun handle(uri: Uri?, fromCamera: Boolean) {
        val type = targetType ?: return
        if (uri == null) return
        scope.launch {
            val doc = withContext(Dispatchers.IO) {
                reader.read(uri, type.lowercase()).also { if (fromCamera) reader.discardCapture() }
            }
            unreadable = doc == null
            if (doc != null) vm.upload(type, doc)
        }
    }

    var captureUri by rememberSaveable { mutableStateOf<Uri?>(null) }
    val camera = rememberLauncherForActivityResult(ActivityResultContracts.TakePicture()) { ok ->
        if (ok) handle(captureUri, fromCamera = true) else reader.discardCapture()
    }
    val photoPicker = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri -> handle(uri, false) }
    val pdfPicker = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) { uri -> handle(uri, false) }

    Text(stringResource(R.string.kyb_documents_intro))
    state.requiredDocuments.forEach { type ->
        val upload = state.uploads[type]
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                Text(stringResource(documentLabel(type)), style = MaterialTheme.typography.titleSmall)
                when (upload?.status) {
                    UploadStatus.UPLOADING -> {
                        LinearProgressIndicator(progress = { upload.percent / 100f }, modifier = Modifier.fillMaxWidth())
                        Text(stringResource(R.string.kyb_uploading, upload.percent))
                    }
                    UploadStatus.RECEIVED -> Text(stringResource(R.string.kyb_received))
                    UploadStatus.FAILED -> TextButton(onClick = { vm.retryUpload(type) }) { Text(stringResource(R.string.kyb_retry_upload)) }
                    null -> Unit
                }
                if (upload?.status != UploadStatus.UPLOADING) {
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(onClick = {
                            targetType = type
                            captureUri = reader.newCaptureUri().also { camera.launch(it) }
                        }) { Text(stringResource(R.string.kyb_take_photo)) }
                        OutlinedButton(onClick = {
                            targetType = type
                            photoPicker.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly))
                        }) { Text(stringResource(R.string.kyb_pick_photo)) }
                    }
                    if (type in PDF_ALLOWED) {
                        TextButton(onClick = { targetType = type; pdfPicker.launch("application/pdf") }) {
                            Text(stringResource(R.string.kyb_pick_pdf))
                        }
                    }
                }
            }
        }
    }
    if (unreadable) Text(stringResource(R.string.kyb_unreadable), color = MaterialTheme.colorScheme.error)
    Button(onClick = vm::submit, enabled = state.canSubmit, modifier = Modifier.fillMaxWidth()) {
        Text(stringResource(R.string.kyb_submit))
    }
    if (state.isBusy) CircularProgressIndicator()
}

@Composable
private fun KybStatusView(state: KybUiState, vm: KybViewModel) {
    val status = state.status ?: return
    Text(stringResource(statusLabel(status.status)), style = MaterialTheme.typography.titleLarge)
    Text(stringResource(statusExplanation(status.status)))
    if (status.status == KybStatusValue.REJECTED && !status.reason.isNullOrBlank()) {
        Text(stringResource(R.string.kyb_reason, status.reason), color = MaterialTheme.colorScheme.error)
    }
    if (status.checks.list69b == "LISTED") Text(stringResource(R.string.kyb_69b_listed), color = MaterialTheme.colorScheme.error)
    if (!state.isOwner && !status.isApproved) Text(stringResource(R.string.kyb_owner_only))
    if (state.isOwner) {
        when (status.status) {
            KybStatusValue.REJECTED, KybStatusValue.DRAFT -> Button(onClick = vm::edit, modifier = Modifier.fillMaxWidth()) {
                Text(stringResource(R.string.kyb_fix_and_resubmit))
            }
        }
    }
}

private val PDF_ALLOWED = setOf(KybDocumentType.PROOF_OF_ADDRESS, KybDocumentType.BANK_STATEMENT, KybDocumentType.CSF)

private fun documentLabel(type: String): Int = when (type) {
    KybDocumentType.INE_FRONT -> R.string.kyb_doc_ine_front
    KybDocumentType.INE_BACK -> R.string.kyb_doc_ine_back
    KybDocumentType.SELFIE -> R.string.kyb_doc_selfie
    KybDocumentType.PROOF_OF_ADDRESS -> R.string.kyb_doc_proof_of_address
    KybDocumentType.BANK_STATEMENT -> R.string.kyb_doc_bank_statement
    KybDocumentType.CSF -> R.string.kyb_doc_csf
    else -> R.string.kyb_doc_other
}

internal fun statusLabel(status: String): Int = when (status) {
    KybStatusValue.DRAFT -> R.string.kyb_status_draft
    KybStatusValue.SUBMITTED -> R.string.kyb_status_submitted
    KybStatusValue.IN_REVIEW -> R.string.kyb_status_in_review
    KybStatusValue.APPROVED -> R.string.kyb_status_approved
    KybStatusValue.REJECTED -> R.string.kyb_status_rejected
    else -> R.string.kyb_status_draft
}

private fun statusExplanation(status: String): Int = when (status) {
    KybStatusValue.SUBMITTED, KybStatusValue.IN_REVIEW -> R.string.kyb_status_review_text
    KybStatusValue.APPROVED -> R.string.kyb_status_approved_text
    KybStatusValue.REJECTED -> R.string.kyb_status_rejected_text
    else -> R.string.kyb_status_draft_text
}

private fun KybError.messageRes(): Int = when (this) {
    KybError.NO_MERCHANT -> R.string.error_no_merchant
    KybError.NOT_OWNER -> R.string.kyb_owner_only
    KybError.NETWORK -> R.string.error_network
    KybError.TOO_LARGE -> R.string.kyb_too_large
    KybError.INCOMPLETE -> R.string.kyb_incomplete
    KybError.NOT_AVAILABLE -> R.string.error_not_available
    KybError.UNKNOWN -> R.string.error_unknown
}
