package mx.softpos.app.core.security

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class DeviceAttestationGuardTest {

    @Test
    fun `verifyForNfcTap returns Allowed when all PCI MPoC security signals pass`() {
        val signals = DeviceSecuritySignals(
            isRooted = false,
            isDebuggerAttached = false,
            hasMemoryHooks = false,
            isHardwareKeystoreBacked = true,
            meetsStrongIntegrity = true,
            attestationToken = "eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.valid_token"
        )

        val verdict = DeviceAttestationGuard.verifyForNfcTap(signals)
        assertTrue(verdict is AttestationVerdict.Allowed)
        assertEquals("eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.valid_token", (verdict as AttestationVerdict.Allowed).attestationToken)
    }

    @Test
    fun `verifyForNfcTap blocks and collects all violations in declaration order`() {
        val compromisedSignals = DeviceSecuritySignals(
            isRooted = true,
            isDebuggerAttached = true,
            hasMemoryHooks = true,
            isHardwareKeystoreBacked = false,
            meetsStrongIntegrity = false,
            attestationToken = "   "
        )

        val verdict = DeviceAttestationGuard.verifyForNfcTap(compromisedSignals)
        assertTrue(verdict is AttestationVerdict.Blocked)
        val blocked = verdict as AttestationVerdict.Blocked

        assertEquals(
            listOf(
                SecurityViolation.ROOT_DETECTED,
                SecurityViolation.DEBUGGER_ATTACHED,
                SecurityViolation.MEMORY_HOOK_DETECTED,
                SecurityViolation.KEYSTORE_NOT_HARDWARE_BACKED,
                SecurityViolation.PLAY_INTEGRITY_FAILED,
                SecurityViolation.BLANK_ATTESTATION_TOKEN
            ),
            blocked.violations
        )
    }
}
