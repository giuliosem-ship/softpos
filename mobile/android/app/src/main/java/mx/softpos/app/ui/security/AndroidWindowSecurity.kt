package mx.softpos.app.ui.security

import android.accessibilityservice.AccessibilityServiceInfo
import android.view.Window
import android.view.accessibility.AccessibilityManager

/** [SecureWindow] backed by a real Android [Window] (FLAG_SECURE etc.). */
class AndroidSecureWindow(private val window: Window) : SecureWindow {
    override fun addFlags(flags: Int) = window.addFlags(flags)
    override fun clearFlags(flags: Int) = window.clearFlags(flags)
    override fun hasFlag(flags: Int): Boolean = (window.attributes.flags and flags) == flags
}

/** [AccessibilityScanner] that reads the enabled services from the system. */
class AndroidAccessibilityScanner(private val manager: AccessibilityManager) : AccessibilityScanner {
    override fun getEnabledServices(): List<AccessibilityServiceDescriptor> =
        manager.getEnabledAccessibilityServiceList(AccessibilityServiceInfo.FEEDBACK_ALL_MASK)
            .map { info ->
                val caps = info.capabilities
                AccessibilityServiceDescriptor(
                    id = info.id,
                    packageName = info.resolveInfo?.serviceInfo?.packageName.orEmpty(),
                    isFeedbackTypeSpoken =
                        (info.feedbackType and AccessibilityServiceInfo.FEEDBACK_SPOKEN) != 0,
                    canRetrieveWindowContent =
                        (caps and AccessibilityServiceInfo.CAPABILITY_CAN_RETRIEVE_WINDOW_CONTENT) != 0,
                    canRequestFilterKeyEvents =
                        (caps and AccessibilityServiceInfo.CAPABILITY_CAN_REQUEST_FILTER_KEY_EVENTS) != 0
                )
            }
}
