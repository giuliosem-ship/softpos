// SoftPOS Mobile POS App Controller

document.addEventListener('DOMContentLoaded', async () => {
  const { SoftPOSSDK } = window;
  const client = new SoftPOSSDK.SoftPOSClient('');
  const nfcService = new SoftPOSSDK.TapToPhoneService();

  // State
  let currentMerchantId = 'merch_demo_01';
  let currentMerchantClabe = '646180123400000007';
  let enteredDigits = '116000'; // Default $1,160.00 MXN for quick demo
  let activeCoDiPayload = null;
  let activePaymentId = null;
  let codiTimerInterval = null;

  // DOM Elements - Navigation & Screens
  const screenKeypad = document.getElementById('screen-keypad');
  const screenNFC = document.getElementById('screen-nfc');
  const screenCoDi = document.getElementById('screen-codi');
  const screenReceipt = document.getElementById('screen-receipt');

  // DOM Elements - Keypad
  const displayAmount = document.getElementById('display-amount');
  const inputConcept = document.getElementById('input-concept');
  const togglePM = document.getElementById('toggle-persona-moral');
  const keypadButtons = document.querySelectorAll('.key-btn');
  const btnPayNFC = document.getElementById('btn-pay-nfc');
  const btnPayCoDi = document.getElementById('btn-pay-codi');

  // DOM Elements - Header Wallet
  const merchantNameLabel = document.getElementById('merchant-name-label');
  const balAvailable = document.getElementById('bal-available');
  const balEscrow = document.getElementById('bal-escrow');
  const balRBF = document.getElementById('bal-rbf');

  // DOM Elements - NFC Screen
  const nfcStatusLabel = document.getElementById('nfc-status-label');
  const selectCardProfile = document.getElementById('select-card-profile');
  const btnSimulateTap = document.getElementById('btn-simulate-tap');
  const btnCancelNFC = document.getElementById('btn-cancel-nfc');
  const nfcSpinnerContainer = document.getElementById('nfc-spinner-container');

  // DOM Elements - CoDi Screen
  const codiAmountBanner = document.getElementById('codi-amount-banner');
  const codiQRContainer = document.getElementById('codi-qr-container');
  const codiCLABEDisplay = document.getElementById('codi-clabe-display');
  const codiTimer = document.getElementById('codi-timer');
  const btnCopyCLABE = document.getElementById('btn-copy-clabe');
  const btnSimulateCoDiConfirm = document.getElementById('btn-simulate-codi-confirm');
  const btnCancelCoDi = document.getElementById('btn-cancel-codi');

  // DOM Elements - Receipt Screen
  const receiptPaymentId = document.getElementById('receipt-payment-id');
  const recGross = document.getElementById('rec-gross');
  const recBase = document.getElementById('rec-base');
  const recIVA = document.getElementById('rec-iva');
  const recISR = document.getElementById('rec-isr');
  const recISRLabel = document.getElementById('rec-isr-label');
  const recRBF = document.getElementById('rec-rbf');
  const recRBFRow = document.getElementById('rec-rbf-row');
  const recNet = document.getElementById('rec-net');
  const btnStampCFDI = document.getElementById('btn-stamp-cfdi');
  const cfdiStampBox = document.getElementById('cfdi-stamp-box');
  const cfdiUUIDLabel = document.getElementById('cfdi-uuid-label');
  const btnNewSale = document.getElementById('btn-new-sale');

  // Helper: Format amount digits to cents integer
  function getAmountCents() {
    return parseInt(enteredDigits, 10) || 0;
  }

  function updateDisplay() {
    const cents = getAmountCents();
    displayAmount.textContent = SoftPOSSDK.formatMXN(cents);
  }

  function showScreen(screenEl) {
    [screenKeypad, screenNFC, screenCoDi, screenReceipt].forEach(s => s.classList.add('hidden'));
    screenEl.classList.remove('hidden');
  }

  // Refresh merchant balances from backend
  async function refreshBalances() {
    try {
      const merchant = await client.getMerchant(currentMerchantId).catch(() => null);
      if (merchant) {
        merchantNameLabel.textContent = `${merchant.name} (${merchant.rfc})`;
        if (merchant.clabe) currentMerchantClabe = merchant.clabe;
      } else {
        merchantNameLabel.textContent = 'Comercio SoftPOS Demo';
      }

      const bals = await client.getMerchantBalances(currentMerchantId);
      balAvailable.textContent = SoftPOSSDK.formatMXN(bals.available_cents);
      balEscrow.textContent = SoftPOSSDK.formatMXN(bals.tax_escrow_cents);
      balRBF.textContent = SoftPOSSDK.formatMXN(bals.rbf_debt_cents);
    } catch (e) {
      console.warn('Balance refresh warning:', e);
    }
  }

  // Keypad Click Handlers
  keypadButtons.forEach(btn => {
    btn.addEventListener('click', () => {
      const key = btn.dataset.key;
      if (key === 'C') {
        enteredDigits = '0';
      } else if (key === '00') {
        if (enteredDigits !== '0' && enteredDigits.length < 10) {
          enteredDigits += '00';
        }
      } else {
        if (enteredDigits === '0') {
          enteredDigits = key;
        } else if (enteredDigits.length < 10) {
          enteredDigits += key;
        }
      }
      updateDisplay();
    });
  });

  // Pay via NFC Contactless
  btnPayNFC.addEventListener('click', async () => {
    const cents = getAmountCents();
    if (cents <= 0) {
      alert('Ingresa un monto válido mayor a $0.00');
      return;
    }

    showScreen(screenNFC);
    nfcSpinnerContainer.style.display = 'none';
    btnSimulateTap.disabled = false;

    // Check Web NFC support
    if (nfcService.isNFCSupported) {
      nfcStatusLabel.textContent = 'Lector NFC nativo activo. Acerca tarjeta o dispositivo.';
      await nfcService.startNFCScan({
        onReading: async ({ tlvHex }) => {
          await processCardTap(tlvHex, cents);
        },
        onError: (err) => {
          nfcStatusLabel.textContent = `NFC: ${err.message}`;
        },
      });
    } else {
      nfcStatusLabel.textContent = 'Modo simulación EMV Contactless L2 listo.';
    }
  });

  async function processCardTap(tlvHex, cents) {
    try {
      nfcSpinnerContainer.style.display = 'block';
      btnSimulateTap.disabled = true;

      activePaymentId = `emv_${Date.now()}`;

      // 1. Authorize EMV Contactless Cryptogram (ARQC)
      await client.authorizeEMV({
        paymentId: activePaymentId,
        merchantId: currentMerchantId,
        tlvHex: tlvHex,
      });

      // 2. Settle payment with atomic RESICO tax split & RBF holdback
      const isPM = togglePM.checked;
      const settleResp = await client.settlePayment({
        paymentId: activePaymentId,
        merchantId: currentMerchantId,
        grossAmountCents: cents,
        isPersonaMoral: isPM,
        platformFeeCents: Math.round(cents * 0.02), // 2.0% platform fee
      });

      renderReceipt(settleResp);
      await refreshBalances();
    } catch (err) {
      alert(`Error al procesar cobro NFC: ${err.message}`);
      showScreen(screenKeypad);
    } finally {
      nfcService.stopNFCScan();
      nfcSpinnerContainer.style.display = 'none';
    }
  }

  btnSimulateTap.addEventListener('click', async () => {
    const cardIdx = parseInt(selectCardProfile.value, 10) || 0;
    const cents = getAmountCents();
    const tapped = nfcService.simulateTap(cardIdx, cents);
    await processCardTap(tapped.tlvHex, cents);
  });

  btnCancelNFC.addEventListener('click', () => {
    nfcService.stopNFCScan();
    showScreen(screenKeypad);
  });

  // Pay via CoDi / SPEI Dynamic QR
  btnPayCoDi.addEventListener('click', async () => {
    const cents = getAmountCents();
    if (cents <= 0) {
      alert('Ingresa un monto válido mayor a $0.00');
      return;
    }

    try {
      const ref = `CODI${Date.now()}`;
      const concept = inputConcept.value.trim() || 'Venta SoftPOS';

      const resp = await client.createCoDiQR({
        merchantId: currentMerchantId,
        sellerClabe: currentMerchantClabe,
        amountCents: cents,
        concept: concept,
        reference: ref,
        ttlSeconds: 900, // 15 mins
      });

      activeCoDiPayload = resp;
      codiAmountBanner.textContent = `${SoftPOSSDK.formatMXN(cents)} • Ref: ${ref}`;
      codiCLABEDisplay.textContent = resp.seller_clabe || currentMerchantClabe;

      // Render Dynamic SVG QR
      const svg = SoftPOSSDK.generateQRCodeSVG(resp.qr_payload, 240);
      codiQRContainer.innerHTML = svg;

      startCoDiCountdown(900);
      showScreen(screenCoDi);
    } catch (err) {
      alert(`Error al generar QR CoDi: ${err.message}`);
    }
  });

  function startCoDiCountdown(seconds) {
    if (codiTimerInterval) clearInterval(codiTimerInterval);
    let remaining = seconds;

    function tick() {
      const m = Math.floor(remaining / 60).toString().padStart(2, '0');
      const s = (remaining % 60).toString().padStart(2, '0');
      codiTimer.textContent = `Expira en: ${m}:${s}`;
      if (remaining <= 0) {
        clearInterval(codiTimerInterval);
        codiTimer.textContent = 'Código QR Expirado';
      }
      remaining--;
    }
    tick();
    codiTimerInterval = setInterval(tick, 1000);
  }

  btnCopyCLABE.addEventListener('click', () => {
    const clabe = codiCLABEDisplay.textContent;
    navigator.clipboard.writeText(clabe).then(() => {
      btnCopyCLABE.textContent = '¡Copiado!';
      setTimeout(() => { btnCopyCLABE.textContent = 'Copiar'; }, 2000);
    });
  });

  btnSimulateCoDiConfirm.addEventListener('click', async () => {
    if (!activeCoDiPayload) return;
    try {
      const trackingKey = `BNX${Date.now()}`;
      const confirmResp = await client.simulateCoDiConfirmation({
        trackingKey: trackingKey,
        sellerClabe: activeCoDiPayload.seller_clabe,
        amountCents: activeCoDiPayload.amount_cents,
      });

      if (codiTimerInterval) clearInterval(codiTimerInterval);

      // Render confirmation receipt
      receiptPaymentId.textContent = `Tracking Banxico: ${confirmResp.tracking_key}`;
      recGross.textContent = SoftPOSSDK.formatMXN(confirmResp.gross_amount_cents);

      // Split detail
      const base = Math.round(confirmResp.gross_amount_cents / 1.16);
      const iva = confirmResp.gross_amount_cents - base;
      recBase.textContent = SoftPOSSDK.formatMXN(base);
      recIVA.textContent = SoftPOSSDK.formatMXN(iva);
      recISRLabel.textContent = 'Apartado ISR RESICO:';
      recISR.textContent = SoftPOSSDK.formatMXN(confirmResp.tax_escrow_cents - iva);
      recRBF.textContent = SoftPOSSDK.formatMXN(confirmResp.rbf_sweep_cents);
      recNet.textContent = SoftPOSSDK.formatMXN(confirmResp.net_available_cents);

      cfdiStampBox.style.display = 'none';
      btnStampCFDI.disabled = false;
      btnStampCFDI.style.display = 'block';

      showScreen(screenReceipt);
      await refreshBalances();
    } catch (err) {
      alert(`Error al confirmar CoDi: ${err.message}`);
    }
  });

  btnCancelCoDi.addEventListener('click', () => {
    if (codiTimerInterval) clearInterval(codiTimerInterval);
    showScreen(screenKeypad);
  });

  // Render Receipt Screen
  function renderReceipt(settle) {
    receiptPaymentId.textContent = `ID de Pago: ${settle.payment_id}`;
    recGross.textContent = SoftPOSSDK.formatMXN(settle.gross_amount_cents);
    recBase.textContent = SoftPOSSDK.formatMXN(settle.base_amount_cents);
    recIVA.textContent = SoftPOSSDK.formatMXN(settle.iva_amount_cents);

    if (settle.is_persona_moral_withholding) {
      recISRLabel.textContent = 'Retención 1.25% ISR PM (Art. 113-J):';
      recISR.textContent = SoftPOSSDK.formatMXN(settle.isr_escrow_cents);
    } else {
      recISRLabel.textContent = `Apartado ISR RESICO (${(settle.isr_rate * 100).toFixed(1)}%):`;
      recISR.textContent = SoftPOSSDK.formatMXN(settle.isr_escrow_cents);
    }

    if (settle.rbf_sweep_cents > 0) {
      recRBFRow.style.display = 'flex';
      recRBF.textContent = SoftPOSSDK.formatMXN(settle.rbf_sweep_cents);
    } else {
      recRBFRow.style.display = 'none';
    }

    recNet.textContent = SoftPOSSDK.formatMXN(settle.net_available_cents);
    cfdiStampBox.style.display = 'none';
    btnStampCFDI.disabled = false;
    btnStampCFDI.style.display = 'block';

    showScreen(screenReceipt);
  }

  // Instant CFDI 4.0 Stamping
  btnStampCFDI.addEventListener('click', async () => {
    try {
      btnStampCFDI.disabled = true;
      btnStampCFDI.textContent = 'Timbrando con PAC...';

      const cents = getAmountCents();
      const cfdiResp = await client.stampCFDI({
        merchantId: currentMerchantId,
        grossAmountCents: cents,
      });

      cfdiStampBox.style.display = 'block';
      cfdiUUIDLabel.textContent = `UUID SAT: ${cfdiResp.uuid}`;
      btnStampCFDI.style.display = 'none';
    } catch (err) {
      btnStampCFDI.disabled = false;
      btnStampCFDI.textContent = 'Reintentar Timbrado CFDI 4.0';
      alert(`Falla de timbrado CFDI: ${err.message}`);
    }
  });

  btnNewSale.addEventListener('click', () => {
    enteredDigits = '0';
    updateDisplay();
    showScreen(screenKeypad);
  });

  // Initial Load
  updateDisplay();
  await refreshBalances();
});
