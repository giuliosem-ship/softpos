package mx.softpos.app.ui.theme

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

private val KernoGreen = Color(0xFF0B3D2E)
private val KernoMint = Color(0xFF3DDC97)
private val KernoSand = Color(0xFFF4EFE6)

private val LightColors = lightColorScheme(
    primary = KernoGreen,
    onPrimary = Color.White,
    secondary = KernoMint,
    onSecondary = KernoGreen,
    background = KernoSand,
    surface = Color.White
)

private val DarkColors = darkColorScheme(
    primary = KernoMint,
    onPrimary = KernoGreen,
    secondary = KernoMint,
    background = Color(0xFF0F1512),
    surface = Color(0xFF17201C)
)

@Composable
fun KernoTheme(darkTheme: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = if (darkTheme) DarkColors else LightColors,
        content = content
    )
}
