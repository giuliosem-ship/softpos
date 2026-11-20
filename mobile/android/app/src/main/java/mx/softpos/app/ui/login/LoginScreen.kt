package mx.softpos.app.ui.login

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R

@Composable
fun LoginScreen(viewModel: LoginViewModel) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    Column(
        modifier = Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterVertically),
        horizontalAlignment = Alignment.CenterHorizontally
    ) {
        Text(stringResource(R.string.app_name), style = MaterialTheme.typography.displaySmall)
        Text(stringResource(R.string.app_tagline), style = MaterialTheme.typography.bodyLarge)

        when (state.step) {
            LoginStep.PHONE -> {
                OutlinedTextField(
                    value = state.phoneInput,
                    onValueChange = viewModel::onPhoneChange,
                    label = { Text(stringResource(R.string.login_phone_label)) },
                    placeholder = { Text(stringResource(R.string.login_phone_hint)) },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
                    modifier = Modifier.fillMaxWidth()
                )
                Button(
                    onClick = viewModel::requestOtp,
                    enabled = !state.isLoading,
                    modifier = Modifier.fillMaxWidth()
                ) { Text(stringResource(R.string.login_send_code)) }
            }
            LoginStep.CODE -> {
                Text(stringResource(R.string.login_code_sent_to, state.phoneE164.orEmpty()))
                OutlinedTextField(
                    value = state.codeInput,
                    onValueChange = viewModel::onCodeChange,
                    label = { Text(stringResource(R.string.login_code_label)) },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword),
                    modifier = Modifier.fillMaxWidth()
                )
                Button(
                    onClick = viewModel::verifyOtp,
                    enabled = !state.isLoading,
                    modifier = Modifier.fillMaxWidth()
                ) { Text(stringResource(R.string.login_verify)) }
                TextButton(onClick = viewModel::backToPhone) { Text(stringResource(R.string.login_change_phone)) }
            }
        }

        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error) }
    }
}

private fun LoginError.messageRes(): Int = when (this) {
    LoginError.INVALID_PHONE -> R.string.login_error_invalid_phone
    LoginError.INVALID_CODE_FORMAT -> R.string.login_error_code_format
    LoginError.RATE_LIMITED -> R.string.login_error_rate_limited
    LoginError.OTP_INVALID -> R.string.login_error_otp_invalid
    LoginError.OTP_LOCKED -> R.string.login_error_otp_locked
    LoginError.NETWORK -> R.string.error_network
    LoginError.UNKNOWN -> R.string.error_unknown
}
