package mx.softpos.app

import android.app.Application

class KernoApplication : Application() {
    val container: AppContainer by lazy { AppContainer(this) }
}
