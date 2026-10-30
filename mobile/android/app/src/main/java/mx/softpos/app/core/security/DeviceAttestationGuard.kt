package mx.softpos.app.core.security

import kotlinx.coroutines.flow.StateFlow

enum class SecurityViolation {
    ROOT_DETECTED,
    DEBUGGER_ATTACHED,
    MEMORY_HOOK_DETECTED,
    KEYSTORE_NOT_HARDWARE_BACKED,
    PLAY_INTEGRITY_FAILED,
    BLANK_ATTESTATION_TOKEN
}

data class DeviceSecuritySignals(
    val isRooted: Boolean,
    val isDebuggerAttached: Boolean,
    val hasMemoryHooks: Boolean,
    val isHardwareKeystoreBacked: Boolean,
    val meetsStrongIntegrity: Boolean,
    val attestationToken: String
)

sealed interface AttestationVerdict {
    data class Allowed(val attestationToken: String) : AttestationVerdict
    data class Blocked(val violations: List<SecurityViolation>) : AttestationVerdict
}

object DeviceAttestationGuard {
    fun verifyForNfcTap(signals: DeviceSecuritySignals): AttestationVerdict {
        val violations = mutableListOf<SecurityViolation>()

        if (signals.isRooted) violations.add(SecurityViolation.ROOT_DETECTED)
        if (signals.isDebuggerAttached) violations.add(SecurityViolation.DEBUGGER_ATTACHED)
        if (signals.hasMemoryHooks) violations.add(SecurityViolation.MEMORY_HOOK_DETECTED)
        if (!signals.isHardwareKeystoreBacked) violations.add(SecurityViolation.KEYSTORE_NOT_HARDWARE_BACKED)
        if (!signals.meetsStrongIntegrity) violations.add(SecurityViolation.PLAY_INTEGRITY_FAILED)
        if (signals.attestationToken.isBlank()) violations.add(SecurityViolation.BLANK_ATTESTATION_TOKEN)

        return if (violations.isEmpty()) {
            AttestationVerdict.Allowed(signals.attestationToken)
        } else {
            AttestationVerdict.Blocked(violations)
        }
    }
}
