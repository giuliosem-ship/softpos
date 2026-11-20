package mx.softpos.app.ui.onboarding

/** Client-side format checks for the KYB form (the backend re-validates everything). */
object KybValidators {
    private val CURP = Regex(
        "^[A-Z][AEIOUX][A-Z]{2}\\d{2}(0[1-9]|1[0-2])(0[1-9]|[12]\\d|3[01])[HMX]" +
            "(AS|BC|BS|CC|CL|CM|CS|CH|DF|DG|GT|GR|HG|JC|MC|MN|MS|NT|NL|OC|PL|QT|QR|SP|SL|SR|TC|TS|TL|VZ|YN|ZS|NE)" +
            "[B-DF-HJ-NP-TV-Z]{3}[A-Z\\d]\\d$"
    )
    private val EMAIL = Regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]{2,}$")
    private val POSTAL_CODE = Regex("^\\d{5}$")
    private val TAX_REGIME = Regex("^\\d{3}$")
    private val MCC = Regex("^\\d{4}$")

    fun isValidRfc(rfc: String) = KybOnboardingPresenter.isValidRfc(rfc)
    fun isPersonaFisica(rfc: String) = rfc.trim().length == 13
    fun isValidCurp(curp: String) = CURP.matches(curp.trim().uppercase())
    fun isValidEmail(email: String) = EMAIL.matches(email.trim())
    fun isValidPostalCode(cp: String) = POSTAL_CODE.matches(cp.trim())
    /** SAT c_RegimenFiscal, e.g. 626 (RESICO), 612, 601. */
    fun isValidTaxRegime(code: String) = TAX_REGIME.matches(code.trim())
    fun isValidMcc(mcc: String) = MCC.matches(mcc.trim())
}
