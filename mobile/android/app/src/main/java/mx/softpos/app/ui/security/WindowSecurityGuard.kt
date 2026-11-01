package mx.softpos.app.ui.security

interface SecureWindow {
    fun addFlags(flags: Int)
    fun clearFlags(flags: Int)
    fun hasFlag(flags: Int): Boolean
}

data class AccessibilityServiceDescriptor(
    val id: String,
    val packageName: String,
    val isFeedbackTypeSpoken: Boolean = false,
    val canRetrieveWindowContent: Boolean = false,
    val canRequestFilterKeyEvents: Boolean = false
)

interface AccessibilityScanner {
    fun getEnabledServices(): List<AccessibilityServiceDescriptor>
}

data class AccessibilitySecurityResult(
    val isSecure: Boolean,
    val detectedThreats: List<String>
)

object WindowSecurityGuard {
    const val FLAG_SECURE: Int = 0x00002000

    val DEFAULT_ALLOWLIST: Set<String> = setOf(
        "com.google.android.marvin.talkback",
        "com.samsung.android.accessibility.talkback",
        "com.android.switchaccess"
    )

    fun protectPinEntry(
        window: SecureWindow,
        scanner: AccessibilityScanner,
        allowlist: Set<String> = DEFAULT_ALLOWLIST
    ): AccessibilitySecurityResult {
        window.addFlags(FLAG_SECURE)

        val threats = mutableListOf<String>()
        val services = scanner.getEnabledServices()

        for (svc in services) {
            val isAllowlisted = allowlist.contains(svc.packageName)
            if (!isAllowlisted) {
                if (svc.canRetrieveWindowContent || svc.canRequestFilterKeyEvents) {
                    threats.add("UNAUTHORIZED_ACCESSIBILITY_INTERCEPTOR: ${svc.id} (${svc.packageName})")
                }
            }
        }

        return AccessibilitySecurityResult(
            isSecure = threats.isEmpty(),
            detectedThreats = threats
        )
    }

    fun releasePinEntry(window: SecureWindow) {
        window.clearFlags(FLAG_SECURE)
    }
}
