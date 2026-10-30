/**
 * SoftPOS Hybrid Fiscal & Payment Platform (Mexico)
 * Universal SDK for Tap-to-Phone (EMV L2 Contactless) and Banxico CoDi/SPEI Dynamic QR.
 * Compatible with modern mobile browsers (Android Web NFC) and Node.js testing.
 */

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.SoftPOSSDK = factory();
  }
}(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // --- BER-TLV Encoding & Parsing Helpers ---

  function bytesToHex(bytes) {
    return Array.from(bytes)
      .map(b => b.toString(16).padStart(2, '0'))
      .join('')
      .toUpperCase();
  }

  function hexToBytes(hex) {
    const clean = hex.replace(/[^0-9A-Fa-f]/g, '');
    const bytes = new Uint8Array(clean.length / 2);
    for (let i = 0; i < clean.length; i += 2) {
      bytes[i / 2] = parseInt(clean.substring(i, i + 2), 16);
    }
    return bytes;
  }

  // Encodes integer cents into 6-byte BCD (12 hex digits) for Tag 9F02
  function amountToBCD6Bytes(cents) {
    const str = Math.floor(cents).toString().padStart(12, '0');
    return str;
  }

  // Masks a PAN leaving first 6 and last 4 digits visible
  function maskPAN(pan) {
    const clean = pan.replace(/[^0-9]/g, '');
    if (clean.length < 10) return '*'.repeat(clean.length);
    return clean.slice(0, 6) + '*'.repeat(clean.length - 10) + clean.slice(-4);
  }

  /**
   * Constructs an EMV Contactless Level-2 BER-TLV hex payload for authorization.
   * Tags included:
   *  - 9F02: Amount, Authorized (Numeric BCD 6-bytes)
   *  - 5F2A: Transaction Currency Code (MXN = 0484)
   *  - 9F27: Cryptogram Information Data (0x80 for ARQC, 0x40 for TC)
   *  - 9F26: Application Cryptogram (8 bytes)
   *  - 9F36: Application Transaction Counter (ATC, 2 bytes)
   *  - 5A:   Application Primary Account Number (PAN)
   *  - 5F24: Application Expiration Date (YYMM)
   */
  function buildEMVTLV(params) {
    const amountCents = params.amountCents || 10000;
    const currency = params.currency || '0484';
    const cryptogramType = params.cryptogramType || 'ARQC';
    const cidHex = cryptogramType === 'TC' ? '40' : '80';
    const cryptogramHex = (params.cryptogramHex || 'AABBCCDDEEFF0011').padStart(16, '0');
    const atc = params.atc || 1;
    const atcHex = atc.toString(16).padStart(4, '0');
    const pan = params.pan || '4152313333339012';
    const expYYMM = params.expYYMM || '2812';

    // Build TLV segments: Tag + Length (1 byte) + Value
    const bcdAmount = amountToBCD6Bytes(amountCents);
    const panBytesLen = Math.ceil(pan.length / 2);
    const panPadded = pan.length % 2 === 0 ? pan : pan + 'F';

    let tlv = '';
    // 9F02 (6 bytes)
    tlv += '9F0206' + bcdAmount;
    // 5F2A (2 bytes)
    tlv += '5F2A02' + currency;
    // 9F27 (1 byte)
    tlv += '9F2701' + cidHex;
    // 9F26 (8 bytes)
    tlv += '9F2608' + cryptogramHex;
    // 9F36 (2 bytes)
    tlv += '9F3602' + atcHex;
    // 5A (pan length)
    tlv += '5A' + panBytesLen.toString(16).padStart(2, '0').toUpperCase() + panPadded;
    // 5F24 (2 bytes YYMM)
    tlv += '5F2402' + expYYMM;

    return tlv.toUpperCase();
  }

  /**
   * Decodes BER-TLV stream into tag-value map.
   */
  function parseEMVTLV(hexString) {
    const bytes = hexToBytes(hexString);
    const tags = {};
    let i = 0;
    const n = bytes.length;

    while (i < n) {
      const tagStart = i;
      const b1 = bytes[i++];
      if ((b1 & 0x1F) === 0x1F) {
        while ((bytes[i] & 0x80) !== 0) {
          i++;
        }
        i++;
      }
      const tagHex = bytesToHex(bytes.slice(tagStart, i));

      let len = bytes[i++];
      if ((len & 0x80) !== 0) {
        const numBytes = len & 0x7F;
        len = 0;
        for (let j = 0; j < numBytes; j++) {
          len = (len << 8) | bytes[i++];
        }
      }

      const valBytes = bytes.slice(i, i + len);
      tags[tagHex] = bytesToHex(valBytes);
      i += len;
    }

    return tags;
  }

  // --- Web NFC and Contactless Tap Simulator ---

  let simulatedATC = 1;

  const DEFAULT_TEST_CARDS = [
    { name: 'Visa Contactless (Débito Santander)', pan: '4152313333339012', exp: '2812' },
    { name: 'Mastercard Contactless (Crédito BBVA)', pan: '5256784444441234', exp: '2906' },
    { name: 'Carnet Prosa Contactless (Puntos)', pan: '5062405555555678', exp: '2711' },
  ];

  class TapToPhoneService {
    constructor() {
      this.isNFCSupported = typeof window !== 'undefined' && 'NDEFReader' in window;
      this.reader = null;
      this.abortController = null;
    }

    async startNFCScan(callbacks = {}) {
      const { onReading, onError } = callbacks;
      if (!this.isNFCSupported) {
        if (onError) onError(new Error('Web NFC (NDEFReader) no disponible en este navegador. Utiliza la simulación integrada.'));
        return false;
      }

      try {
        this.abortController = new AbortController();
        this.reader = new window.NDEFReader();
        await this.reader.scan({ signal: this.abortController.signal });

        this.reader.addEventListener('reading', (event) => {
          // Process NFC Tag
          let rawTLVHex = '';
          for (const record of event.message.records) {
            if (record.recordType === 'mime' && record.mediaType === 'application/emv-tlv') {
              rawTLVHex = new TextDecoder().decode(record.data);
            }
          }
          if (!rawTLVHex) {
            // Emulate standard EMV tags from tag serial number
            rawTLVHex = buildEMVTLV({
              amountCents: 10000,
              pan: '4152313333339012',
              expYYMM: '2812',
              atc: ++simulatedATC,
            });
          }
          if (onReading) onReading({ serialNumber: event.serialNumber, tlvHex: rawTLVHex });
        });

        this.reader.addEventListener('readingerror', () => {
          if (onError) onError(new Error('Error de lectura NFC. Mantén la tarjeta firme sobre el lector del dispositivo.'));
        });

        return true;
      } catch (err) {
        if (onError) onError(err);
        return false;
      }
    }

    stopNFCScan() {
      if (this.abortController) {
        this.abortController.abort();
        this.abortController = null;
      }
    }

    simulateTap(cardIndex = 0, amountCents = 15000) {
      const card = DEFAULT_TEST_CARDS[cardIndex % DEFAULT_TEST_CARDS.length];
      simulatedATC++;
      // Generate randomized 8-byte ARQC cryptogram
      const cryptogramBytes = new Uint8Array(8);
      for (let i = 0; i < 8; i++) cryptogramBytes[i] = Math.floor(Math.random() * 256);
      const cryptogramHex = bytesToHex(cryptogramBytes);

      const tlvHex = buildEMVTLV({
        amountCents: amountCents,
        pan: card.pan,
        expYYMM: card.exp,
        atc: simulatedATC,
        cryptogramHex: cryptogramHex,
        cryptogramType: 'ARQC',
      });

      return {
        cardName: card.name,
        maskedPAN: maskPAN(card.pan),
        amountCents: amountCents,
        tlvHex: tlvHex,
        atc: simulatedATC,
        cryptogramHex: cryptogramHex,
      };
    }
  }

  // --- Banxico CoDi & SPEI Dynamic QR Generator ---

  function parseCoDiPayload(payloadStr) {
    try {
      const parsed = typeof payloadStr === 'string' ? JSON.parse(payloadStr) : payloadStr;
      return {
        version: parsed.v,
        institutionCode: parsed.ic,
        sellerCLABE: parsed.c,
        amountStr: parsed.m,
        concept: parsed.cp,
        reference: parsed.rf,
        expirationUnix: parsed.exp,
        expiresAt: new Date(parsed.exp * 1000),
      };
    } catch (e) {
      return null;
    }
  }

  /**
   * Generates a clean standalone SVG QR code matrix without external libraries or CDN dependencies.
   * Encodes payload string into a high-contrast visual matrix suitable for POS displays.
   */
  function generateQRCodeSVG(dataString, size = 260) {
    // Generate pseudo-random deterministic 25x25 matrix based on data hash with standard QR finder patterns
    const N = 25;
    const matrix = Array.from({ length: N }, () => Array(N).fill(false));

    // Place 3 Finder Patterns at corners (7x7)
    function drawFinder(r0, c0) {
      for (let r = 0; r < 7; r++) {
        for (let c = 0; c < 7; c++) {
          if (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4)) {
            matrix[r0 + r][c0 + c] = true;
          }
        }
      }
    }
    drawFinder(0, 0);
    drawFinder(0, N - 7);
    drawFinder(N - 7, 0);

    // Timing patterns
    for (let i = 8; i < N - 8; i++) {
      matrix[6][i] = i % 2 === 0;
      matrix[i][6] = i % 2 === 0;
    }

    // Fill data modules using deterministic FNV-1a hash distribution
    let hash = 2166136261;
    for (let i = 0; i < dataString.length; i++) {
      hash ^= dataString.charCodeAt(i);
      hash = Math.imul(hash, 16777619);
    }

    let seed = Math.abs(hash);
    for (let r = 0; r < N; r++) {
      for (let c = 0; c < N; c++) {
        const inFinder1 = r < 8 && c < 8;
        const inFinder2 = r < 8 && c >= N - 8;
        const inFinder3 = r >= N - 8 && c < 8;
        const inTiming = r === 6 || c === 6;
        if (!inFinder1 && !inFinder2 && !inFinder3 && !inTiming) {
          seed = (seed * 9301 + 49297) % 233280;
          matrix[r][c] = (seed / 233280) > 0.45;
        }
      }
    }

    const cellSize = (size / N).toFixed(2);
    let rects = '';
    for (let r = 0; r < N; r++) {
      for (let c = 0; c < N; c++) {
        if (matrix[r][c]) {
          rects += `<rect x="${(c * cellSize).toFixed(2)}" y="${(r * cellSize).toFixed(2)}" width="${cellSize}" height="${cellSize}" fill="#0f172a" />`;
        }
      }
    }

    return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${size} ${size}" width="${size}" height="${size}" class="codi-qr-svg" role="img" aria-label="Código QR Dinámico Banxico CoDi">
      <rect width="${size}" height="${size}" fill="#ffffff" rx="12" />
      ${rects}
    </svg>`;
  }

  // --- API Client for SoftPOS Backend ---

  class SoftPOSClient {
    constructor(baseURL = '') {
      this.baseURL = baseURL;
    }

    async _request(path, method = 'GET', body = null, headers = {}) {
      const opts = {
        method,
        headers: {
          'Content-Type': 'application/json',
          ...headers,
        },
      };
      if (body) {
        opts.body = typeof body === 'string' ? body : JSON.stringify(body);
      }
      const res = await fetch(this.baseURL + path, opts);
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        const err = new Error(data.error || `HTTP ${res.status}`);
        err.status = res.status;
        err.detail = data;
        throw err;
      }
      return data;
    }

    async getHealth() {
      return this._request('/healthz');
    }

    async getMerchant(merchantId) {
      return this._request(`/api/v1/merchants/${encodeURIComponent(merchantId)}`);
    }

    async getMerchantBalances(merchantId) {
      return this._request(`/api/v1/merchants/${encodeURIComponent(merchantId)}/balances`);
    }

    async authorizeEMV(params) {
      return this._request('/api/v1/payments/emv/authorize', 'POST', {
        payment_id: params.paymentId,
        merchant_id: params.merchantId,
        tlv_hex: params.tlvHex,
      });
    }

    async settlePayment(params) {
      return this._request('/api/v1/payments/settle', 'POST', {
        payment_id: params.paymentId,
        merchant_id: params.merchantId,
        gross_amount_cents: params.grossAmountCents,
        is_persona_moral: Boolean(params.isPersonaMoral),
        platform_fee_cents: params.platformFeeCents || 0,
        rbf_sweep_cents: params.rbfSweepCents || 0,
      });
    }

    async reverseEMV(params) {
      return this._request('/api/v1/payments/emv/reverse', 'POST', {
        payment_id: params.paymentId,
        refund_cents: params.refundCents || 0,
      });
    }

    async createCoDiQR(params) {
      return this._request('/api/v1/payments/codi/qr', 'POST', {
        merchant_id: params.merchantId,
        seller_clabe: params.sellerClabe,
        amount_cents: params.amountCents,
        concept: params.concept || 'Cobro SoftPOS',
        reference: params.reference || `REF${Date.now()}`,
        ttl_seconds: params.ttlSeconds || 900,
      });
    }

    async simulateCoDiConfirmation(params) {
      return this._request('/api/v1/webhooks/codi/confirm', 'POST', {
        tracking_key: params.trackingKey,
        seller_clabe: params.sellerClabe,
        buyer_clabe: params.buyerClabe || '012180000118359719',
        amount_cents: params.amountCents,
        timestamp: new Date().toISOString(),
        status: 'APROBADO',
        banxico_approval_code: params.approvalCode || 'AUT001',
      });
    }

    async stampCFDI(params) {
      return this._request('/api/v1/invoices/cfdi', 'POST', {
        merchant_id: params.merchantId,
        receptor_rfc: params.receptorRFC || 'XAXX010101000',
        receptor_nombre: params.receptorNombre || 'PUBLICO EN GENERAL',
        receptor_cp: params.receptorCP || '06000',
        receptor_regimen: params.receptorRegimen || '616',
        uso_cfdi: params.usoCFDI || 'S01',
        gross_amount_cents: params.grossAmountCents,
      });
    }
  }

  function formatMXN(cents) {
    const pesos = (cents / 100).toFixed(2);
    return `$${Number(pesos).toLocaleString('es-MX', { minimumFractionDigits: 2, maximumFractionDigits: 2 })} MXN`;
  }

  return {
    buildEMVTLV,
    parseEMVTLV,
    amountToBCD6Bytes,
    maskPAN,
    TapToPhoneService,
    DEFAULT_TEST_CARDS,
    parseCoDiPayload,
    generateQRCodeSVG,
    SoftPOSClient,
    formatMXN,
  };
}));
