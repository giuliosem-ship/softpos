package mx.softpos.app

import android.os.Bundle
import android.view.accessibility.AccessibilityManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import mx.softpos.app.ui.KernoApp
import mx.softpos.app.ui.security.AndroidAccessibilityScanner
import mx.softpos.app.ui.security.AndroidSecureWindow
import mx.softpos.app.ui.security.WindowSecurityGuard
import mx.softpos.app.ui.theme.KernoTheme

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Every screen shows balances, sales or payment data: FLAG_SECURE
        // (no screenshots / recents thumbnails) is applied app-wide.
        WindowSecurityGuard.protectPinEntry(
            window = AndroidSecureWindow(window),
            scanner = AndroidAccessibilityScanner(getSystemService(AccessibilityManager::class.java))
        )
        enableEdgeToEdge()
        val container = (application as KernoApplication).container
        setContent {
            KernoTheme {
                KernoApp(container)
            }
        }
    }
}
