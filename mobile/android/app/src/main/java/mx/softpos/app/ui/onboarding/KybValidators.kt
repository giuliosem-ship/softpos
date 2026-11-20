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
    private val MCC = Regex("^\\d{4}$")

    fun isValidRfc(rfc: String) = KybOnboardingPresenter.isValidRfc(rfc)
    fun isPersonaFisica(rfc: String) = rfc.trim().length == 13
    fun isValidCurp(curp: String) = CURP.matches(curp.trim().uppercase())
    fun isValidEmail(email: String) = EMAIL.matches(email.trim())
    fun isValidPostalCode(cp: String) = POSTAL_CODE.matches(cp.trim())
    /**
     * Backend `kyb.TaxRegime` values (pkg/kyb knownRegimes), not the SAT numeric
     * c_RegimenFiscal key. Only RESICO_PF passes the submission checks in the MVP;
     * the others are accepted by PUT and rejected at submit (UNSUPPORTED_TAX_REGIME).
     */
    const val RESICO_PF = "RESICO_PF"
    val TAX_REGIMES = listOf(RESICO_PF, "RESICO_PM", "ACTIVIDAD_EMPRESARIAL_PF", "PLATAFORMAS_TECNOLOGICAS_PF", "GENERAL_PM", "OTHER")
    fun isValidTaxRegime(code: String) = code.trim() in TAX_REGIMES
    fun isValidMcc(mcc: String) = MCC.matches(mcc.trim())
}
