package mx.softpos.app.ui.charge

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R
import mx.softpos.app.core.money.MoneyFormatter
import mx.softpos.app.ui.codi.CodiQrMatrix

@Composable
fun ChargeScreen(viewModel: ChargeViewModel) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    Column(
        modifier = Modifier.fillMaxSize().padding(16.dp).verticalScroll(rememberScrollState()),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        when (val step = state.step) {
            ChargeStep.Entering -> AmountEntry(state, viewModel)
            is ChargeStep.ProcessingCard -> Processing(stringResource(step.stage.messageRes()), state.amountCents)
            ChargeStep.LoadingSpei -> Processing(stringResource(R.string.charge_spei_loading), state.amountCents)
            is ChargeStep.CardApproved -> Result(
                title = stringResource(R.string.charge_card_approved),
                amountCents = step.amountCents,
                detail = stringResource(R.string.sale_id, step.paymentId),
                onDone = viewModel::newCharge
            )
            is ChargeStep.CardPending -> Result(
                title = stringResource(R.string.charge_card_pending),
                amountCents = step.amountCents,
                detail = stringResource(R.string.sale_id, step.paymentId),
                onDone = viewModel::newCharge
            )
            is ChargeStep.SpeiQr -> SpeiQrView(step, onDone = viewModel::newCharge, onBack = viewModel::backToAmount)
        }
        state.error?.let { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error, textAlign = TextAlign.Center) }
    }
}

@Composable
private fun AmountEntry(state: ChargeUiState, vm: ChargeViewModel) {
    Text(stringResource(R.string.charge_title), style = MaterialTheme.typography.titleMedium)
    Text(MoneyFormatter.formatMxn(state.amountCents), style = MaterialTheme.typography.displayMedium)
    Keypad(onDigit = vm::onDigit, onBackspace = vm::onBackspace, onClear = vm::onClear)
    Button(
        onClick = vm::chargeWithCard,
        enabled = state.amountCents > 0 && state.tapAvailable,
        modifier = Modifier.fillMaxWidth().height(56.dp)
    ) { Text(stringResource(R.string.charge_method_card)) }
    if (!state.tapAvailable) Text(stringResource(R.string.charge_error_tap_unavailable), style = MaterialTheme.typography.bodySmall)
    FilledTonalButton(
        onClick = vm::chargeWithSpei,
        enabled = state.amountCents > 0,
        modifier = Modifier.fillMaxWidth().height(56.dp)
    ) { Text(stringResource(R.string.charge_method_spei)) }
}

@Composable
private fun Keypad(onDigit: (Int) -> Unit, onBackspace: () -> Unit, onClear: () -> Unit) {
    val rows = listOf(listOf(1, 2, 3), listOf(4, 5, 6), listOf(7, 8, 9))
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        rows.forEach { row ->
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                row.forEach { d -> KeyButton(d.toString(), Modifier.weight(1f)) { onDigit(d) } }
            }
        }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            KeyButton(stringResource(R.string.keypad_clear), Modifier.weight(1f), onClear)
            KeyButton("0", Modifier.weight(1f)) { onDigit(0) }
            KeyButton(stringResource(R.string.keypad_backspace), Modifier.weight(1f), onBackspace)
        }
    }
}

@Composable
private fun KeyButton(label: String, modifier: Modifier, onClick: () -> Unit) {
    OutlinedButton(onClick = onClick, modifier = modifier.height(56.dp)) {
        Text(label, style = MaterialTheme.typography.titleLarge)
    }
}

@Composable
private fun Processing(message: String, amountCents: Long) {
    Text(MoneyFormatter.formatMxn(amountCents), style = MaterialTheme.typography.displaySmall)
    CircularProgressIndicator()
    Text(message, textAlign = TextAlign.Center)
}

@Composable
private fun Result(title: String, amountCents: Long, detail: String, onDone: () -> Unit) {
    Text(title, style = MaterialTheme.typography.headlineSmall)
    Text(MoneyFormatter.formatMxn(amountCents), style = MaterialTheme.typography.displaySmall)
    Text(detail, style = MaterialTheme.typography.bodySmall)
    Button(onClick = onDone, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.charge_new)) }
}

@Composable
private fun SpeiQrView(step: ChargeStep.SpeiQr, onDone: () -> Unit, onBack: () -> Unit) {
    Text(stringResource(R.string.charge_spei_title), style = MaterialTheme.typography.titleMedium)
    Text(MoneyFormatter.formatMxn(step.amountCents), style = MaterialTheme.typography.displaySmall)
    QrCode(step.qr, Modifier.fillMaxWidth(0.8f))
    Text(stringResource(R.string.charge_spei_clabe, step.clabe))
    if (step.beneficiary.isNotBlank()) Text(stringResource(R.string.charge_spei_beneficiary, step.beneficiary))
    Text(stringResource(R.string.charge_spei_reference, step.reference))
    Text(stringResource(R.string.charge_spei_hint), style = MaterialTheme.typography.bodySmall, textAlign = TextAlign.Center)
    Button(onClick = onDone, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.charge_new)) }
    TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
}

@Composable
private fun QrCode(matrix: CodiQrMatrix, modifier: Modifier = Modifier) {
    Canvas(modifier = modifier.aspectRatio(1f).background(Color.White)) {
        val cell = size.width / matrix.width
        for (y in 0 until matrix.height) {
            for (x in 0 until matrix.width) {
                if (matrix.getModule(x, y)) {
                    drawRect(Color.Black, topLeft = Offset(x * cell, y * cell), size = Size(cell, cell))
                }
            }
        }
    }
}

private fun CardStage.messageRes(): Int = when (this) {
    CardStage.CREATING_INTENT -> R.string.charge_card_creating
    CardStage.WAITING_FOR_CARD -> R.string.charge_card_waiting
    CardStage.CONFIRMING -> R.string.charge_card_confirming
}

private fun ChargeError.messageRes(): Int = when (this) {
    ChargeError.NO_AMOUNT -> R.string.charge_error_no_amount
    ChargeError.NO_MERCHANT -> R.string.error_no_merchant
    ChargeError.TAP_UNAVAILABLE -> R.string.charge_error_tap_unavailable
    ChargeError.NO_CLABE -> R.string.charge_error_no_clabe
    ChargeError.DECLINED -> R.string.charge_error_declined
    ChargeError.CANCELLED -> R.string.charge_error_cancelled
    ChargeError.NETWORK -> R.string.error_network
    ChargeError.NOT_AVAILABLE -> R.string.error_not_available
    ChargeError.UNKNOWN -> R.string.error_unknown
}
