package mx.softpos.app.ui.merchant

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Card
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import mx.softpos.app.R

@Composable
fun LoadErrorMessage(error: LoadError, onRetry: (() -> Unit)?) {
    Column(
        modifier = Modifier.fillMaxWidth().padding(16.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(8.dp)
    ) {
        Text(stringResource(error.messageRes()), color = MaterialTheme.colorScheme.error)
        if (onRetry != null && error != LoadError.NO_MERCHANT) {
            TextButton(onClick = onRetry) { Text(stringResource(R.string.action_retry)) }
        }
    }
}

@Composable
fun InfoCard(title: String, value: String, modifier: Modifier = Modifier, caption: String? = null) {
    Card(modifier = modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(title, style = MaterialTheme.typography.labelLarge)
            Text(value, style = MaterialTheme.typography.headlineMedium)
            if (caption != null) Text(caption, style = MaterialTheme.typography.bodySmall)
        }
    }
}

fun LoadError.messageRes(): Int = when (this) {
    LoadError.NO_MERCHANT -> R.string.error_no_merchant
    LoadError.NETWORK -> R.string.error_network
    LoadError.NOT_AVAILABLE -> R.string.error_not_available
    LoadError.UNKNOWN -> R.string.error_unknown
}
