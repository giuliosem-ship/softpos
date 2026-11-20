package mx.softpos.app.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.material3.Button
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import mx.softpos.app.AppContainer
import mx.softpos.app.R
import mx.softpos.app.ui.login.LoginScreen
import mx.softpos.app.ui.login.LoginViewModel

/** Root: the session decides between the login flow and the merchant app. */
@Composable
fun KernoApp(container: AppContainer) {
    val session by container.sessionManager.session.collectAsStateWithLifecycle()
    Surface(modifier = Modifier.fillMaxSize().safeDrawingPadding()) {
        if (session == null) {
            val vm = viewModel { LoginViewModel(container.authApi, container.sessionManager) }
            LoginScreen(vm)
        } else {
            Column(
                modifier = Modifier.fillMaxSize(),
                verticalArrangement = Arrangement.Center,
                horizontalAlignment = Alignment.CenterHorizontally
            ) {
                Text(stringResource(R.string.session_signed_in))
                Button(onClick = container::logout) { Text(stringResource(R.string.action_logout)) }
            }
        }
    }
}
