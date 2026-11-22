package mx.softpos.app.ui.merchant

import android.content.ActivityNotFoundException
import android.content.Intent
import androidx.compose.ui.platform.LocalContext
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.foundation.layout.Row
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.text.input.KeyboardType
import mx.softpos.app.core.money.MoneyParser
import mx.softpos.app.payments.CardPayment
import mx.softpos.app.payments.CardPaymentStatus
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R
import mx.softpos.app.core.money.MoneyFormatter
import mx.softpos.app.payments.Sale
import mx.softpos.app.payments.SaleKind
import mx.softpos.app.payments.SaleStatus
import mx.softpos.app.payments.SaleStatusGroup

@Composable
fun SalesScreen(
    viewModel: SalesViewModel,
    onOpen: (String) -> Unit,
    onInvoice: (List<String>) -> Unit = {},
    onInvoices: () -> Unit = {}
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }

    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(stringResource(R.string.sales_title), style = MaterialTheme.typography.headlineSmall)
        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { LoadErrorMessage(it, viewModel::load) }
        if (!state.isLoading && state.error == null && state.sales.isEmpty()) {
            Text(stringResource(R.string.sales_empty))
        }
        Row {
            TextButton(onClick = viewModel::load) { Text(stringResource(R.string.action_refresh)) }
            TextButton(onClick = onInvoices) { Text(stringResource(R.string.invoices_title)) }
            if (state.isSelecting) {
                TextButton(onClick = viewModel::cancelSelection) { Text(stringResource(R.string.sales_select_cancel)) }
            } else {
                TextButton(onClick = viewModel::startSelection) { Text(stringResource(R.string.sales_select)) }
            }
        }
        val selection = state.selection
        if (selection != null) {
            Button(
                onClick = { onInvoice(state.sales.map { it.id }.filter { it in selection }) },
                enabled = selection.isNotEmpty(),
                modifier = Modifier.fillMaxWidth()
            ) { Text(stringResource(R.string.invoice_selected, selection.size)) }
        }
        LazyColumn(Modifier.weight(1f)) {
            items(state.sales, key = { it.id }) { sale ->
                ListItem(
                    headlineContent = { Text(MoneyFormatter.formatMxn(sale.amountCents)) },
                    supportingContent = { Text(stringResource(sale.kindRes()) + " · " + stringResource(statusRes(sale.status))) },
                    trailingContent = { Text(sale.createdAt.take(10)) },
                    leadingContent = selection?.let { sel ->
                        {
                            Checkbox(
                                checked = sale.id in sel,
                                onCheckedChange = { viewModel.toggle(sale.id) },
                                enabled = sale.isInvoiceable()
                            )
                        }
                    },
                    modifier = Modifier.clickable { if (selection != null) viewModel.toggle(sale.id) else onOpen(sale.id) }
                )
                HorizontalDivider()
            }
            if (state.nextCursor != null) {
                item(key = "more") {
                    // Reaching the end of the list asks for the next page.
                    LaunchedEffect(state.nextCursor) { viewModel.loadMore() }
                    if (state.isLoadingMore) CircularProgressIndicator(Modifier.padding(16.dp))
                    else TextButton(onClick = viewModel::loadMore) { Text(stringResource(R.string.sales_load_more)) }
                }
            }
        }
    }
}

@Composable
fun SaleDetailScreen(viewModel: SaleDetailViewModel, onBack: () -> Unit, onInvoice: ((String) -> Unit)? = null) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }
    var confirm by remember { mutableStateOf<RefundChoice?>(null) }

    Column(
        Modifier.fillMaxSize().padding(16.dp).verticalScroll(rememberScrollState()),
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        val sale = state.sale
        if (sale == null) {
            Text(stringResource(R.string.sale_not_found))
        } else {
            val status = state.payment?.status ?: sale.status
            InfoCard(
                title = stringResource(R.string.sale_amount),
                value = MoneyFormatter.formatMxn(sale.amountCents),
                caption = stringResource(sale.kindRes()) + " · " + stringResource(statusRes(status))
            )
            Text(stringResource(R.string.sale_id, sale.id))
            if (sale.createdAt.isNotBlank()) Text(stringResource(R.string.sale_date, sale.createdAt))
            sale.card?.let { card ->
                card.last4?.let { Text(stringResource(R.string.sale_card, card.brand.uppercase(), it)) }
                if (card.authCode.isNotBlank()) Text(stringResource(R.string.sale_auth_code, card.authCode))
            }
            if (onInvoice != null && sale.copy(status = status).isInvoiceable()) {
                OutlinedButton(onClick = { onInvoice(sale.id) }, modifier = Modifier.fillMaxWidth()) {
                    Text(stringResource(R.string.invoice_action))
                }
            }
        }
        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { LoadErrorMessage(it, viewModel::load) }
        state.payment?.let { p -> CardPaymentDetail(p) }
        state.lastRefund?.let {
            Text(stringResource(R.string.refund_requested, MoneyFormatter.formatMxn(it.amountCents)))
        }
        state.receiptUrl?.let { url -> ReceiptSection(state, viewModel, url) }
        if (state.canRefund) {
            RefundSection(state, viewModel, onConfirm = { confirm = it })
        }
        TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
    }

    confirm?.let { choice ->
        val amount = when (choice) {
            RefundChoice.ALL -> state.refundableCents
            RefundChoice.PARTIAL -> MoneyParser.parseMxnToCents(state.refundInput) ?: 0L
        }
        AlertDialog(
            onDismissRequest = { confirm = null },
            title = { Text(stringResource(R.string.refund_confirm_title)) },
            text = { Text(stringResource(R.string.refund_confirm_text, MoneyFormatter.formatMxn(amount))) },
            confirmButton = {
                TextButton(onClick = {
                    confirm = null
                    if (choice == RefundChoice.ALL) viewModel.refundAll() else viewModel.refundPartial()
                }) { Text(stringResource(R.string.refund_confirm)) }
            },
            dismissButton = { TextButton(onClick = { confirm = null }) { Text(stringResource(R.string.action_back)) } }
        )
    }
}

private enum class RefundChoice { ALL, PARTIAL }

@Composable
private fun CardPaymentDetail(p: CardPayment) {
    if (p.capturedCents > 0L) Text(stringResource(R.string.sale_captured, MoneyFormatter.formatMxn(p.capturedCents)))
    if (p.refundedCents > 0L) Text(stringResource(R.string.sale_refunded, MoneyFormatter.formatMxn(p.refundedCents)))
    if (p.status == CardPaymentStatus.CHARGEBACK) {
        Text(
            stringResource(R.string.sale_chargeback_open, MoneyFormatter.formatMxn(p.chargebackCents)),
            color = MaterialTheme.colorScheme.error
        )
    }
    if (p.status == CardPaymentStatus.CHARGEBACK_REVERSED) Text(stringResource(R.string.sale_chargeback_reversed))
    if (p.declineCode.isNotBlank()) Text(stringResource(R.string.sale_decline_code, p.declineCode))
    p.settlement?.let {
        Text(stringResource(R.string.sale_fee, MoneyFormatter.formatMxn(it.platformFeeCents)))
        Text(stringResource(R.string.sale_tax_set_aside, MoneyFormatter.formatMxn(it.taxEscrowCents)))
        Text(stringResource(R.string.sale_net, MoneyFormatter.formatMxn(it.netAvailableCents)))
    }
}

@Composable
private fun ReceiptSection(state: SaleDetailUiState, vm: SaleDetailViewModel, url: String) {
    val context = LocalContext.current
    val amount = state.sale?.let { MoneyFormatter.formatMxn(it.amountCents) }.orEmpty()
    val shareText = stringResource(R.string.receipt_share_text, amount, url)
    val subject = stringResource(R.string.receipt_share_subject)
    val chooserTitle = stringResource(R.string.receipt_share)
    Text(stringResource(R.string.receipt_title), style = MaterialTheme.typography.titleMedium)
    state.sale?.receiptExpiresAt?.takeIf { it.isNotBlank() }?.let { Text(stringResource(R.string.receipt_expires, it.take(10))) }
    Button(
        onClick = {
            // Plain text share sheet (WhatsApp, SMS, email…): only the public link, no tenant data.
            val send = Intent(Intent.ACTION_SEND).apply {
                type = "text/plain"
                putExtra(Intent.EXTRA_SUBJECT, subject)
                putExtra(Intent.EXTRA_TEXT, shareText)
            }
            try {
                context.startActivity(Intent.createChooser(send, chooserTitle))
            } catch (_: ActivityNotFoundException) {
                // nothing to share with
            }
        },
        modifier = Modifier.fillMaxWidth()
    ) { Text(stringResource(R.string.receipt_share)) }
    if (!state.canSendReceipt) return
    OutlinedTextField(
        value = state.receiptEmail,
        onValueChange = vm::onReceiptEmail,
        label = { Text(stringResource(R.string.receipt_email_label)) },
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email),
        singleLine = true,
        modifier = Modifier.fillMaxWidth()
    )
    OutlinedTextField(
        value = state.receiptPhone,
        onValueChange = vm::onReceiptPhone,
        label = { Text(stringResource(R.string.receipt_phone_label)) },
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
        singleLine = true,
        modifier = Modifier.fillMaxWidth()
    )
    OutlinedButton(
        onClick = vm::sendReceipt,
        enabled = !state.isSendingReceipt,
        modifier = Modifier.fillMaxWidth()
    ) { Text(stringResource(R.string.receipt_send)) }
    if (state.isSendingReceipt) CircularProgressIndicator()
    state.receiptSent?.forEach { (channel, ok) ->
        val name = stringResource(if (channel == "sms") R.string.receipt_channel_sms else R.string.receipt_channel_email)
        Text(
            stringResource(if (ok) R.string.receipt_sent_ok else R.string.receipt_sent_failed, name),
            color = if (ok) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.error
        )
    }
    state.receiptError?.let { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error) }
}

private fun ReceiptError.messageRes(): Int = when (this) {
    ReceiptError.NO_DESTINATION -> R.string.receipt_error_no_destination
    ReceiptError.INVALID_EMAIL -> R.string.receipt_error_email
    ReceiptError.INVALID_PHONE -> R.string.receipt_error_phone
    ReceiptError.NOT_CONFIRMED -> R.string.receipt_error_not_confirmed
    ReceiptError.NOT_ALLOWED -> R.string.receipt_error_not_allowed
    ReceiptError.NOT_CONFIGURED -> R.string.receipt_error_not_configured
    ReceiptError.SEND_FAILED -> R.string.receipt_error_send_failed
    ReceiptError.NETWORK -> R.string.error_network
    ReceiptError.UNKNOWN -> R.string.error_unknown
}

@Composable
private fun RefundSection(state: SaleDetailUiState, vm: SaleDetailViewModel, onConfirm: (RefundChoice) -> Unit) {
    Text(stringResource(R.string.refund_title), style = MaterialTheme.typography.titleMedium)
    Text(stringResource(R.string.refund_refundable, MoneyFormatter.formatMxn(state.refundableCents)))
    Button(onClick = { onConfirm(RefundChoice.ALL) }, enabled = !state.isRefunding, modifier = Modifier.fillMaxWidth()) {
        Text(stringResource(R.string.refund_all))
    }
    OutlinedTextField(
        value = state.refundInput,
        onValueChange = vm::onRefundInput,
        label = { Text(stringResource(R.string.refund_amount_label)) },
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Decimal),
        singleLine = true,
        modifier = Modifier.fillMaxWidth()
    )
    OutlinedButton(
        onClick = { onConfirm(RefundChoice.PARTIAL) },
        enabled = !state.isRefunding && state.refundInput.isNotBlank(),
        modifier = Modifier.fillMaxWidth()
    ) { Text(stringResource(R.string.refund_partial)) }
    if (state.isRefunding) CircularProgressIndicator()
    state.refundError?.let { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error) }
}

private fun RefundError.messageRes(): Int = when (this) {
    RefundError.INVALID_AMOUNT -> R.string.refund_error_invalid_amount
    RefundError.EXCEEDS_REMAINING -> R.string.refund_error_exceeds
    RefundError.NOT_ALLOWED -> R.string.refund_error_not_allowed
    RefundError.ACQUIRER_UNAVAILABLE -> R.string.refund_error_acquirer_unavailable
    RefundError.ACQUIRER_REJECTED -> R.string.refund_error_rejected
    RefundError.IDEMPOTENCY_CONFLICT -> R.string.charge_error_idempotency
    RefundError.NETWORK -> R.string.error_network
    RefundError.UNKNOWN -> R.string.error_unknown
}

internal fun Sale.kindRes(): Int = when (kind.lowercase()) {
    SaleKind.SPEI -> R.string.method_spei
    SaleKind.CODI -> R.string.method_codi
    else -> R.string.method_card
}

internal fun statusRes(status: String): Int = when (SaleStatus.group(status)) {
    SaleStatusGroup.APPROVED -> R.string.status_approved
    SaleStatusGroup.FAILED -> R.string.status_failed
    SaleStatusGroup.PENDING -> R.string.status_pending
    SaleStatusGroup.PARTIALLY_REFUNDED -> R.string.status_partially_refunded
    SaleStatusGroup.REFUNDED -> R.string.status_refunded
    SaleStatusGroup.CHARGEBACK -> R.string.status_chargeback
}
