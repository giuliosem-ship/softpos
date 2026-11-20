# Release shrinking rules (R8). Placeholder set for the MVP; review before the
# first Play Console upload.

# kotlinx.serialization: keep the generated serializers of @Serializable classes.
-keepattributes *Annotation*, InnerClasses
-dontnote kotlinx.serialization.**
-keepclassmembers @kotlinx.serialization.Serializable class mx.softpos.app.** {
    *** Companion;
    kotlinx.serialization.KSerializer serializer(...);
}
-keepclasseswithmembers class mx.softpos.app.** {
    kotlinx.serialization.KSerializer serializer(...);
}

# OkHttp ships its own consumer rules; silence optional TLS provider warnings.
-dontwarn org.conscrypt.**
-dontwarn org.bouncycastle.**
-dontwarn org.openjsse.**
