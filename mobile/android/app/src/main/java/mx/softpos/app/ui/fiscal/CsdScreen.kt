package mx.softpos.app.ui.fiscal

import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
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
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R
import mx.softpos.app.fiscal.CsdIssue
import mx.softpos.app.ui.merchant.InfoCard
import mx.softpos.app.ui.merchant.LoadErrorMessage

@Composable
fun CsdScreen(viewModel: CsdViewModel, reader: CsdFileReader, onBack: () -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }
    // Plain remember (not rememberSaveable): the password never reaches the saved
    // instance state. It is handed over as a CharArray and the field is cleared.
    var password by remember { mutableStateOf("") }

    val cerPicker = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) reader.read(uri).let { (name, bytes) -> viewModel.onCerPicked(name, bytes) }
    }
    val keyPicker = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) reader.read(uri).let { (name, bytes) -> viewModel.onKeyPicked(name, bytes) }
    }

    Column(
        Modifier.fillMaxSize().padding(16.dp).verticalScroll(rememberScrollState()),
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        Text(stringResource(R.string.csd_title), style = MaterialTheme.typography.headlineSmall)
        Text(stringResource(R.string.csd_intro))
        if (state.isLoading) CircularProgressIndicator()
        state.loadError?.let { LoadErrorMessage(it, viewModel::load) }

        state.csd?.let { csd ->
            InfoCard(
                title = stringResource(R.string.csd_current),
                value = stringResource(if (csd.isActive) R.string.csd_status_active else R.string.csd_status_inactive),
                caption = stringResource(R.string.csd_serial_rfc, csd.serial, csd.rfc)
            )
            Text(stringResource(R.string.csd_validity, csd.validFrom.take(10), csd.validTo.take(10)))
            when {
                state.expired -> Text(stringResource(R.string.csd_expired), color = MaterialTheme.colorScheme.error)
                state.expiringSoon -> Text(
                    stringResource(R.string.csd_expiring_soon, state.daysLeft ?: 0L),
                    color = MaterialTheme.colorScheme.error
                )
            }
        }
        if (state.notConfigured) Text(stringResource(R.string.csd_not_configured))
        if (state.uploaded) Text(stringResource(R.string.csd_uploaded))

        if (!state.isOwner) {
            Text(stringResource(R.string.csd_owner_only))
        } else {
            Text(stringResource(R.string.csd_upload_title), style = MaterialTheme.typography.titleMedium)
            OutlinedButton(onClick = { cerPicker.launch(arrayOf("*/*")) }, modifier = Modifier.fillMaxWidth()) {
                Text(state.cerName ?: stringResource(R.string.csd_pick_cer))
            }
            state.cerSummary?.let {
                Text(stringResource(R.string.csd_cer_summary, it.serial, it.rfc ?: "—", it.validTo.toString().take(10)))
            }
            OutlinedButton(onClick = { keyPicker.launch(arrayOf("*/*")) }, modifier = Modifier.fillMaxWidth()) {
                Text(state.keyName ?: stringResource(R.string.csd_pick_key))
            }
            OutlinedTextField(
                value = password,
                onValueChange = {
                    password = it
                    viewModel.onPasswordEdited()
                },
                label = { Text(stringResource(R.string.csd_password)) },
                visualTransformation = PasswordVisualTransformation(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, autoCorrectEnabled = false),
                singleLine = true,
                modifier = Modifier.fillMaxWidth()
            )
            state.issues.forEach { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error) }
            Button(
                onClick = {
                    val chars = password.toCharArray()
                    password = ""
                    viewModel.upload(chars)
                },
                enabled = state.canUpload && password.isNotEmpty(),
                modifier = Modifier.fillMaxWidth()
            ) { Text(stringResource(R.string.csd_upload)) }
            if (state.isUploading) CircularProgressIndicator()
            state.uploadError?.let { e ->
                Text(stringResource(e.messageRes()), color = MaterialTheme.colorScheme.error)
                state.invalidReason?.takeIf { it.isNotBlank() }?.let { Text(stringResource(R.string.csd_reason, it)) }
            }
        }
        TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
    }
}

private fun CsdIssue.messageRes(): Int = when (this) {
    CsdIssue.CER_UNREADABLE -> R.string.csd_issue_cer_unreadable
    CsdIssue.CER_EXPIRED -> R.string.csd_issue_cer_expired
    CsdIssue.CER_NOT_YET_VALID -> R.string.csd_issue_cer_not_yet_valid
    CsdIssue.RFC_MISMATCH -> R.string.csd_issue_rfc_mismatch
    CsdIssue.KEY_INVALID -> R.string.csd_issue_key_invalid
    CsdIssue.PASSWORD_REQUIRED -> R.string.csd_issue_password
}

private fun CsdUploadError.messageRes(): Int = when (this) {
    CsdUploadError.INVALID -> R.string.csd_error_invalid
    CsdUploadError.NOT_ALLOWED -> R.string.csd_owner_only
    CsdUploadError.NETWORK -> R.string.error_network
    CsdUploadError.UNKNOWN -> R.string.error_unknown
}
