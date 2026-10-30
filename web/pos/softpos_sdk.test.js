const { test, describe } = require('node:test');
const assert = require('node:assert/strict');
const SoftPOSSDK = require('./softpos_sdk.js');

describe('SoftPOS Universal SDK Suite', () => {

  test('amountToBCD6Bytes converts integer cents to 12 hex digits BCD', () => {
    assert.equal(SoftPOSSDK.amountToBCD6Bytes(25000), '000000025000');
    assert.equal(SoftPOSSDK.amountToBCD6Bytes(116000), '000000116000');
    assert.equal(SoftPOSSDK.amountToBCD6Bytes(1), '000000000001');
  });

  test('maskPAN preserves BIN and last 4 digits while masking middle', () => {
    assert.equal(SoftPOSSDK.maskPAN('4152313333339012'), '415231******9012');
    assert.equal(SoftPOSSDK.maskPAN('5256784444441234'), '525678******1234');
    assert.equal(SoftPOSSDK.maskPAN('1234'), '****');
  });

  test('buildEMVTLV generates valid BER-TLV byte stream for EMV L2 contactless', () => {
    const tlvHex = SoftPOSSDK.buildEMVTLV({
      amountCents: 25000,
      currency: '0484',
      cryptogramType: 'ARQC',
      cryptogramHex: 'AABBCCDDEEFF0011',
      atc: 20,
      pan: '4152313333339012',
      expYYMM: '2812',
    });

    assert.ok(tlvHex.includes('9F0206000000025000'), 'must include 9F02 with BCD 25000');
    assert.ok(tlvHex.includes('5F2A020484'), 'must include 5F2A currency 0484 (MXN)');
    assert.ok(tlvHex.includes('9F270180'), 'must include 9F27 CID 80 for ARQC');
    assert.ok(tlvHex.includes('9F2608AABBCCDDEEFF0011'), 'must include 9F26 Application Cryptogram');
    assert.ok(tlvHex.includes('9F36020014'), 'must include 9F36 ATC 20 (0x0014)');
    assert.ok(tlvHex.includes('5A084152313333339012'), 'must include 5A PAN');
    assert.ok(tlvHex.includes('5F24022812'), 'must include 5F24 Exp 2812');
  });

  test('parseEMVTLV correctly decodes BER-TLV stream into structured map', () => {
    const tlvHex = SoftPOSSDK.buildEMVTLV({
      amountCents: 50000,
      cryptogramType: 'TC',
      cryptogramHex: '1122334455667788',
      atc: 7,
      pan: '5256784444441234',
      expYYMM: '2906',
    });

    const parsed = SoftPOSSDK.parseEMVTLV(tlvHex);
    assert.equal(parsed['9F02'], '000000050000');
    assert.equal(parsed['5F2A'], '0484');
    assert.equal(parsed['9F27'], '40'); // TC
    assert.equal(parsed['9F26'], '1122334455667788');
    assert.equal(parsed['9F36'], '0007');
    assert.equal(parsed['5A'], '5256784444441234');
    assert.equal(parsed['5F24'], '2906');
  });

  test('TapToPhoneService generates realistic contactless tap simulation with incremental ATC', () => {
    const nfc = new SoftPOSSDK.TapToPhoneService();
    const tap1 = nfc.simulateTap(0, 15000);

    assert.equal(tap1.amountCents, 15000);
    assert.equal(tap1.maskedPAN, '415231******9012');
    assert.ok(tap1.tlvHex.length > 50);
    assert.ok(tap1.atc > 0);

    const tap2 = nfc.simulateTap(1, 20000);
    assert.equal(tap2.maskedPAN, '525678******1234');
    assert.equal(tap2.atc, tap1.atc + 1, 'ATC must increment on every tap');
  });

  test('parseCoDiPayload and generateQRCodeSVG produce valid Banxico wire output', () => {
    const rawWire = JSON.stringify({
      v: 1,
      ic: '646',
      c: '646180123400000007',
      m: '116.00',
      cp: 'Orden de tacos',
      rf: 'CODIREF01',
      exp: 1792610000,
    });

    const codi = SoftPOSSDK.parseCoDiPayload(rawWire);
    assert.equal(codi.version, 1);
    assert.equal(codi.institutionCode, '646');
    assert.equal(codi.sellerCLABE, '646180123400000007');
    assert.equal(codi.amountStr, '116.00');
    assert.equal(codi.reference, 'CODIREF01');

    const svg = SoftPOSSDK.generateQRCodeSVG(rawWire, 200);
    assert.ok(svg.includes('<svg'), 'must contain svg root element');
    assert.ok(svg.includes('class="codi-qr-svg"'), 'must contain codi-qr-svg class');
    assert.ok(svg.includes('<rect'), 'must contain rendered rect modules');
  });

  test('formatMXN formats cents cleanly', () => {
    assert.equal(SoftPOSSDK.formatMXN(116000), '$1,160.00 MXN');
    assert.equal(SoftPOSSDK.formatMXN(50), '$0.50 MXN');
    assert.equal(SoftPOSSDK.formatMXN(0), '$0.00 MXN');
  });

});
