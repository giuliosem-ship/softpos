package mx.softpos.app.ui.security

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class WindowSecurityGuardTest {

    private class TestSecureWindow(var flags: Int = 0) : SecureWindow {
        override fun addFlags(flags: Int) {
            this.flags = this.flags or flags
        }

        override fun clearFlags(flags: Int) {
            this.flags = this.flags and flags.inv()
        }

        override fun hasFlag(flags: Int): Boolean {
            return (this.flags and flags) == flags
        }
    }

    private class TestAccessibilityScanner(
        private val services: List<AccessibilityServiceDescriptor>
    ) : AccessibilityScanner {
        override fun getEnabledServices(): List<AccessibilityServiceDescriptor> = services
    }

    @Test
    fun `protectPinEntry enables FLAG_SECURE on window and approves clean environment`() {
        val window = TestSecureWindow()
        val scanner = TestAccessibilityScanner(emptyList())

        assertFalse(window.hasFlag(WindowSecurityGuard.FLAG_SECURE))

        val result = WindowSecurityGuard.protectPinEntry(window, scanner)

        assertTrue(result.isSecure)
        assertTrue(result.detectedThreats.isEmpty())
        assertTrue(window.hasFlag(WindowSecurityGuard.FLAG_SECURE))
    }

    @Test
    fun `protectPinEntry allows approved system screen reader like Google TalkBack`() {
        val window = TestSecureWindow()
        val talkback = AccessibilityServiceDescriptor(
            id = "com.google.android.marvin.talkback/.TalkBackService",
            packageName = "com.google.android.marvin.talkback",
            isFeedbackTypeSpoken = true,
            canRetrieveWindowContent = true,
            canRequestFilterKeyEvents = false
        )
        val scanner = TestAccessibilityScanner(listOf(talkback))

        val result = WindowSecurityGuard.protectPinEntry(window, scanner)

        assertTrue(result.isSecure)
        assertTrue(result.detectedThreats.isEmpty())
        assertTrue(window.hasFlag(WindowSecurityGuard.FLAG_SECURE))
    }

    @Test
    fun `protectPinEntry blocks suspicious accessibility service capable of window retrieval`() {
        val window = TestSecureWindow()
        val maliciousKeylogger = AccessibilityServiceDescriptor(
            id = "com.evil.spyware/.KeyloggerService",
            packageName = "com.evil.spyware",
            isFeedbackTypeSpoken = false,
            canRetrieveWindowContent = true,
            canRequestFilterKeyEvents = true
        )
        val scanner = TestAccessibilityScanner(listOf(maliciousKeylogger))

        val result = WindowSecurityGuard.protectPinEntry(window, scanner)

        assertFalse(result.isSecure)
        assertEquals(1, result.detectedThreats.size)
        assertTrue(result.detectedThreats[0].contains("com.evil.spyware"))
        assertTrue(window.hasFlag(WindowSecurityGuard.FLAG_SECURE))
    }

    @Test
    fun `releasePinEntry removes FLAG_SECURE from window`() {
        val window = TestSecureWindow(flags = WindowSecurityGuard.FLAG_SECURE)
        assertTrue(window.hasFlag(WindowSecurityGuard.FLAG_SECURE))

        WindowSecurityGuard.releasePinEntry(window)

        assertFalse(window.hasFlag(WindowSecurityGuard.FLAG_SECURE))
    }
}
