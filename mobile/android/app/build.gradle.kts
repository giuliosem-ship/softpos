plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
}

android {
    // Kotlin package of the code, kept from the pure-logic module so imports and
    // tests stay stable. The store identity is `applicationId` below.
    namespace = "mx.softpos.app"
    // 36 = Play's target requirement for new uploads from 2026-08.
    compileSdk = 36
    buildToolsVersion = "35.0.0"

    defaultConfig {
        applicationId = "mx.kerno.softpos"
        // 28 (Android 9): floor of the Tap to Pay on Android SDKs evaluated for
        // the MVP and of StrongBox/hardware-backed Keystore AES-GCM.
        minSdk = 28
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            // Android emulator alias of the host loopback (backend on :8080).
            buildConfigField("String", "API_BASE_URL", "\"http://10.0.2.2:8080\"")
            // Mock Tap to Pay provider that asks a DEV_MODE backend to play the
            // processor webhooks (POST /api/v1/dev/card-payments/{id}/simulate)
            // until the processor SDK is integrated.
            buildConfigField("boolean", "DEV_TAP_SIMULATOR", "true")
        }
        release {
            // Placeholder until the production gateway exists; HTTPS only.
            buildConfigField("String", "API_BASE_URL", "\"https://api.kerno.mx\"")
            buildConfigField("boolean", "DEV_TAP_SIMULATOR", "false")
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
            // No signingConfig on purpose: release keys are never committed
            // (Play App Signing + CI secret, see docs/ANDROID_APP.md).
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }

    testOptions {
        unitTests {
            isReturnDefaultValues = true
            all { test ->
                // The existing suites are JUnit 5 (Jupiter); AGP local unit
                // tests are plain Gradle Test tasks, so no extra plugin needed.
                test.useJUnitPlatform()
                test.testLogging {
                    events("passed", "skipped", "failed")
                }
            }
        }
    }

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
    }
}

kotlin {
    jvmToolchain(17)
}

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.ui)
    implementation(libs.androidx.ui.tooling.preview)
    implementation(libs.androidx.material3)
    implementation(libs.androidx.material.icons.core)
    implementation(libs.androidx.navigation.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    debugImplementation(libs.androidx.ui.tooling)

    implementation(libs.kotlinx.serialization.json)
    implementation(libs.kotlinx.coroutines.core)
    implementation(libs.kotlinx.coroutines.android)
    implementation(libs.okhttp)
    implementation(libs.zxing.core)

    testImplementation(libs.junit.jupiter.api)
    testRuntimeOnly(libs.junit.jupiter.engine)
    testRuntimeOnly(libs.junit.platform.launcher)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.okhttp.mockwebserver)
}
