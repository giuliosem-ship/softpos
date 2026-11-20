package mx.softpos.app

import android.os.Bundle
import android.view.accessibility.AccessibilityManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
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
        setContent {
            KernoTheme {
                Surface(modifier = Modifier.fillMaxSize()) {
                    Column(
                        modifier = Modifier.fillMaxSize(),
                        verticalArrangement = Arrangement.Center,
                        horizontalAlignment = Alignment.CenterHorizontally
                    ) {
                        Text(stringResource(R.string.app_name), style = MaterialTheme.typography.headlineLarge)
                        Text(stringResource(R.string.app_tagline))
                    }
                }
            }
        }
    }
}
