package mx.softpos.app.ui.fiscal

import android.content.ActivityNotFoundException
import android.content.ClipData
import android.content.Intent
import androidx.core.net.toUri
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import mx.softpos.app.R
import mx.softpos.app.core.money.MoneyFormatter
import mx.softpos.app.fiscal.CancelMotivo
import mx.softpos.app.fiscal.Invoice
import mx.softpos.app.fiscal.InvoiceFileKind
import mx.softpos.app.fiscal.InvoiceStatus
import mx.softpos.app.fiscal.Persona
import mx.softpos.app.ui.merchant.InfoCard
import mx.softpos.app.ui.merchant.LoadErrorMessage

@Composable
fun InvoiceFormScreen(
    viewModel: InvoiceFormViewModel,
    onCsd: () -> Unit,
    onOpenInvoice: (String) -> Unit,
    onBack: () -> Unit
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    // Runs on every visit, so coming back from the CSD screen re-checks the gate.
    LaunchedEffect(Unit) { viewModel.load() }

    Column(
        Modifier.fillMaxSize().padding(16.dp).verticalScroll(rememberScrollState()),
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        Text(stringResource(R.string.invoice_new_title), style = MaterialTheme.typography.headlineSmall)
        Text(stringResource(R.string.invoice_sales_count, state.saleIds.size))

        val result = state.result
        if (result != null) {
            InvoiceResult(result, onOpenInvoice)
            TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
            return@Column
        }

        if (state.csd == CsdGate.MISSING) {
            Text(stringResource(R.string.invoice_csd_required), color = MaterialTheme.colorScheme.error)
            Button(onClick = onCsd, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.settings_csd)) }
        }
        state.catalogError?.let { LoadErrorMessage(it, viewModel::load) }
        if (state.catalogs == null && state.catalogError == null) CircularProgressIndicator()

        val f = state.form
        val invalid = state.invalidFields
        OutlinedTextField(
            value = f.rfc,
            onValueChange = viewModel::onRfc,
            label = { Text(stringResource(R.string.invoice_rfc)) },
            isError = ReceptorField.RFC in invalid,
            supportingText = {
                val persona = state.persona
                Text(
                    when {
                        persona == Persona.FISICA -> stringResource(R.string.invoice_persona_fisica)
                        persona == Persona.MORAL -> stringResource(R.string.invoice_persona_moral)
                        f.rfc.length >= 12 -> stringResource(R.string.invoice_rfc_invalid)
                        else -> ""
                    }
                )
            },
            singleLine = true,
            modifier = Modifier.fillMaxWidth()
        )
        OutlinedTextField(
            value = f.nombre,
            onValueChange = viewModel::onNombre,
            label = { Text(stringResource(R.string.invoice_nombre)) },
            supportingText = { Text(stringResource(R.string.invoice_nombre_hint)) },
            isError = ReceptorField.NOMBRE in invalid,
            singleLine = true,
            modifier = Modifier.fillMaxWidth()
        )
        CatalogPicker(
            label = stringResource(R.string.invoice_regimen),
            selected = state.regimenOptions.firstOrNull { it.code == f.regimen }?.let { "${it.code} · ${it.description}" },
            options = state.regimenOptions.map { it.code to "${it.code} · ${it.description}" },
            enabled = state.persona != null,
            isError = ReceptorField.REGIMEN in invalid,
            onSelect = viewModel::onRegimen
        )
        CatalogPicker(
            label = stringResource(R.string.invoice_uso_cfdi),
            selected = state.usoOptions.firstOrNull { it.code == f.usoCfdi }?.let { "${it.code} · ${it.description}" },
            options = state.usoOptions.map { it.code to "${it.code} · ${it.description}" },
            enabled = f.regimen.isNotEmpty(),
            isError = ReceptorField.USO_CFDI in invalid,
            onSelect = viewModel::onUsoCfdi
        )
        OutlinedTextField(
            value = f.codigoPostal,
            onValueChange = viewModel::onCodigoPostal,
            label = { Text(stringResource(R.string.invoice_cp)) },
            isError = ReceptorField.CODIGO_POSTAL in invalid,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
            singleLine = true,
            modifier = Modifier.fillMaxWidth()
        )
        OutlinedTextField(
            value = f.email,
            onValueChange = viewModel::onEmail,
            label = { Text(stringResource(R.string.invoice_email)) },
            isError = ReceptorField.EMAIL in invalid,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email),
            singleLine = true,
            modifier = Modifier.fillMaxWidth()
        )
        Button(onClick = viewModel::submit, enabled = state.canSubmit, modifier = Modifier.fillMaxWidth()) {
            Text(stringResource(R.string.invoice_submit))
        }
        if (state.isSubmitting) CircularProgressIndicator()
        state.error?.let { e ->
            Text(stringResource(e.messageRes()), color = MaterialTheme.colorScheme.error)
            if (e == InvoiceError.CSD_REQUIRED) TextButton(onClick = onCsd) { Text(stringResource(R.string.settings_csd)) }
        }
        TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
    }
}

@Composable
private fun InvoiceResult(invoice: Invoice, onOpenInvoice: (String) -> Unit) {
    InfoCard(
        title = stringResource(invoiceStatusRes(invoice.status)),
        value = MoneyFormatter.formatMxn(invoice.totalCents),
        caption = invoice.uuid?.let { stringResource(R.string.invoice_uuid, it) }
    )
    when (invoice.status) {
        InvoiceStatus.PENDING -> Text(stringResource(R.string.invoice_pending_text))
        InvoiceStatus.FAILED -> Text(
            stringResource(R.string.invoice_failed_text, invoice.error ?: "—"),
            color = MaterialTheme.colorScheme.error
        )
    }
    Button(onClick = { onOpenInvoice(invoice.invoiceId) }, modifier = Modifier.fillMaxWidth()) {
        Text(stringResource(R.string.invoice_open))
    }
}

@Composable
private fun CatalogPicker(
    label: String,
    selected: String?,
    options: List<Pair<String, String>>,
    enabled: Boolean,
    isError: Boolean,
    onSelect: (String) -> Unit
) {
    var open by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxWidth()) {
        Text(label, style = MaterialTheme.typography.labelLarge, color = if (isError) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface)
        Box {
            OutlinedButton(onClick = { open = true }, enabled = enabled && options.isNotEmpty(), modifier = Modifier.fillMaxWidth()) {
                Text(selected ?: stringResource(R.string.invoice_choose))
            }
            DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
                options.forEach { (code, text) ->
                    DropdownMenuItem(text = { Text(text) }, onClick = {
                        open = false
                        onSelect(code)
                    })
                }
            }
        }
    }
}

@Composable
fun InvoicesScreen(viewModel: InvoicesViewModel, onOpen: (String) -> Unit, onBack: () -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { viewModel.load() }

    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(stringResource(R.string.invoices_title), style = MaterialTheme.typography.headlineSmall)
        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { LoadErrorMessage(it, viewModel::load) }
        if (!state.isLoading && state.error == null && state.invoices.isEmpty()) Text(stringResource(R.string.invoices_empty))
        Row {
            TextButton(onClick = viewModel::load) { Text(stringResource(R.string.action_refresh)) }
            TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
        }
        LazyColumn(Modifier.weight(1f)) {
            items(state.invoices, key = { it.invoiceId }) { inv ->
                ListItem(
                    headlineContent = { Text(MoneyFormatter.formatMxn(inv.totalCents)) },
                    supportingContent = {
                        Text(stringResource(invoiceStatusRes(inv.status)) + " · " + (inv.receptor?.rfc ?: "—"))
                    },
                    trailingContent = { Text(inv.createdAt.take(10)) },
                    modifier = Modifier.clickable { onOpen(inv.invoiceId) }
                )
                HorizontalDivider()
            }
            if (state.nextCursor != null) {
                item(key = "more") {
                    LaunchedEffect(state.nextCursor) { viewModel.loadMore() }
                    if (state.isLoadingMore) CircularProgressIndicator(Modifier.padding(16.dp))
                    else TextButton(onClick = viewModel::loadMore) { Text(stringResource(R.string.sales_load_more)) }
                }
            }
        }
    }
}

@Composable
fun InvoiceDetailScreen(viewModel: InvoiceDetailViewModel, onBack: () -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val context = LocalContext.current
    LaunchedEffect(Unit) { viewModel.load() }
    var confirmCancel by remember { mutableStateOf(false) }
    val chooserTitle = stringResource(R.string.invoice_share)

    LaunchedEffect(state.share) {
        val file = state.share ?: return@LaunchedEffect
        val uri = file.uri.toUri()
        val send = Intent(Intent.ACTION_SEND).apply {
            type = file.mimeType
            putExtra(Intent.EXTRA_STREAM, uri)
            clipData = ClipData.newRawUri("", uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
        try {
            context.startActivity(Intent.createChooser(send, chooserTitle))
        } catch (_: ActivityNotFoundException) {
            // nothing to share with; the file stays in the cache until the next download
        }
        viewModel.consumeShare()
    }

    Column(
        Modifier.fillMaxSize().padding(16.dp).verticalScroll(rememberScrollState()),
        verticalArrangement = Arrangement.spacedBy(12.dp)
    ) {
        if (state.isLoading) CircularProgressIndicator()
        state.error?.let { LoadErrorMessage(it, viewModel::load) }
        state.invoice?.let { inv ->
            InfoCard(
                title = stringResource(invoiceStatusRes(inv.status)),
                value = MoneyFormatter.formatMxn(inv.totalCents),
                caption = listOfNotNull(inv.serie, inv.folio).joinToString("-").ifBlank { null }
            )
            inv.uuid?.let { Text(stringResource(R.string.invoice_uuid, it)) }
            Text(stringResource(R.string.invoice_subtotal, MoneyFormatter.formatMxn(inv.subtotalCents)))
            Text(stringResource(R.string.invoice_iva, MoneyFormatter.formatMxn(inv.ivaCents)))
            inv.receptor?.let { Text(stringResource(R.string.invoice_receptor, it.nombre, it.rfc)) }
            if (inv.createdAt.isNotBlank()) Text(stringResource(R.string.sale_date, inv.createdAt))
            inv.stampedAt?.let { Text(stringResource(R.string.invoice_stamped_at, it)) }
            if (inv.status == InvoiceStatus.FAILED) {
                Text(stringResource(R.string.invoice_failed_text, inv.error ?: "—"), color = MaterialTheme.colorScheme.error)
            }
            if (!inv.uuid.isNullOrBlank()) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(onClick = { viewModel.share(InvoiceFileKind.PDF) }, enabled = state.downloading == null) {
                        Text(stringResource(R.string.invoice_share_pdf))
                    }
                    OutlinedButton(onClick = { viewModel.share(InvoiceFileKind.XML) }, enabled = state.downloading == null) {
                        Text(stringResource(R.string.invoice_share_xml))
                    }
                }
            }
            if (state.downloading != null) CircularProgressIndicator()
            if (state.downloadFailed) Text(stringResource(R.string.invoice_download_failed), color = MaterialTheme.colorScheme.error)
            if (state.canCancel || state.isCanceling) CancelSection(state, viewModel, onConfirm = { confirmCancel = true })
        }
        TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
    }

    if (confirmCancel) {
        AlertDialog(
            onDismissRequest = { confirmCancel = false },
            title = { Text(stringResource(R.string.invoice_cancel_title)) },
            text = { Text(stringResource(R.string.invoice_cancel_confirm_text)) },
            confirmButton = {
                TextButton(onClick = {
                    confirmCancel = false
                    viewModel.cancel()
                }) { Text(stringResource(R.string.invoice_cancel)) }
            },
            dismissButton = { TextButton(onClick = { confirmCancel = false }) { Text(stringResource(R.string.action_back)) } }
        )
    }
}

@Composable
private fun CancelSection(state: InvoiceDetailUiState, vm: InvoiceDetailViewModel, onConfirm: () -> Unit) {
    Text(stringResource(R.string.invoice_cancel_title), style = MaterialTheme.typography.titleMedium)
    CancelMotivo.ALL.forEach { code ->
        Row(
            Modifier.fillMaxWidth().clickable { vm.onMotivo(code) },
            verticalAlignment = Alignment.CenterVertically
        ) {
            RadioButton(selected = state.motivo == code, onClick = { vm.onMotivo(code) })
            Text(stringResource(motivoRes(code)))
        }
    }
    if (state.needsFolio) {
        OutlinedTextField(
            value = state.folioSustitucion,
            onValueChange = vm::onFolio,
            label = { Text(stringResource(R.string.invoice_folio_sustitucion)) },
            isError = state.cancelError == CancelError.FOLIO_REQUIRED,
            singleLine = true,
            modifier = Modifier.fillMaxWidth()
        )
    }
    Button(onClick = onConfirm, enabled = state.canCancel, modifier = Modifier.fillMaxWidth()) {
        Text(stringResource(R.string.invoice_cancel))
    }
    if (state.isCanceling) CircularProgressIndicator()
    state.cancelError?.let { Text(stringResource(it.messageRes()), color = MaterialTheme.colorScheme.error) }
}

private fun motivoRes(code: String): Int = when (code) {
    CancelMotivo.WITH_RELATION -> R.string.invoice_motivo_01
    CancelMotivo.WITHOUT_RELATION -> R.string.invoice_motivo_02
    CancelMotivo.NOT_EXECUTED -> R.string.invoice_motivo_03
    else -> R.string.invoice_motivo_04
}

internal fun invoiceStatusRes(status: String): Int = when (status) {
    InvoiceStatus.STAMPED -> R.string.invoice_status_stamped
    InvoiceStatus.PENDING -> R.string.invoice_status_pending
    InvoiceStatus.FAILED -> R.string.invoice_status_failed
    InvoiceStatus.CANCEL_REQUESTED -> R.string.invoice_status_cancel_requested
    InvoiceStatus.CANCELED -> R.string.invoice_status_canceled
    else -> R.string.invoice_status_pending
}

private fun InvoiceError.messageRes(): Int = when (this) {
    InvoiceError.ALREADY_INVOICED -> R.string.invoice_error_already_invoiced
    InvoiceError.RECEPTOR_INVALID -> R.string.invoice_error_receptor
    InvoiceError.CSD_REQUIRED -> R.string.invoice_csd_required
    InvoiceError.NOT_ALLOWED -> R.string.invoice_error_not_allowed
    InvoiceError.IDEMPOTENCY_CONFLICT -> R.string.invoice_error_idempotency
    InvoiceError.PROVIDER_UNAVAILABLE -> R.string.invoice_error_provider
    InvoiceError.NETWORK -> R.string.error_network
    InvoiceError.UNKNOWN -> R.string.error_unknown
}

private fun CancelError.messageRes(): Int = when (this) {
    CancelError.FOLIO_REQUIRED -> R.string.invoice_error_folio
    CancelError.NOT_ALLOWED -> R.string.invoice_error_cancel_not_allowed
    CancelError.INVALID -> R.string.invoice_error_cancel_invalid
    CancelError.PROVIDER_UNAVAILABLE -> R.string.invoice_error_provider
    CancelError.NETWORK -> R.string.error_network
    CancelError.UNKNOWN -> R.string.error_unknown
}
