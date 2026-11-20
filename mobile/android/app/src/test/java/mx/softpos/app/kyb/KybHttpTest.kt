package mx.softpos.app.kyb

import mx.softpos.app.core.network.ApiResult
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class KybHttpTest {
    private lateinit var server: MockWebServer
    private val client = OkHttpClient()

    @BeforeEach fun setUp() { server = MockWebServer().apply { start() } }
    @AfterEach fun tearDown() { server.shutdown() }

    private fun api() = HttpKybApi(server.url("/").toString(), client)

    @Test
    fun getKybReadsContract() {
        server.enqueue(
            MockResponse().setBody(
                """{"status":"REJECTED","reason":"INE ilegible","checks":{"rfc_valid":true,"list_69b":"CLEAR",
                   "documents_complete":false},"required_documents":["INE_FRONT","CSF"],"submitted_at":"t","decided_at":"u"}"""
            )
        )
        val k = (api().getKyb("mer_1") as ApiResult.Success).data
        assertEquals(KybStatusValue.REJECTED, k.status)
        assertEquals("INE ilegible", k.reason)
        assertEquals("CLEAR", k.checks.list69b)
        assertEquals(listOf("INE_FRONT", "CSF"), k.requiredDocuments)
        assertEquals("/api/v1/merchants/mer_1/kyb", server.takeRequest().path)
    }

    @Test
    fun putSendsApplicationShape() {
        server.enqueue(MockResponse().setBody("""{"status":"DRAFT"}"""))
        val app = KybApplication(
            legalName = "Tacos SA de CV", rfc = "TAC010101AB1", taxRegime = "GENERAL_PM", mcc = "5812",
            address = KybAddress("Reforma", "10", null, "Juárez", "CDMX", "CDMX", "06600"),
            contactEmail = "a@b.mx", contactPhone = "+525512345678"
        )
        assertTrue(api().putKyb("mer_1", app) is ApiResult.Success)
        val req = server.takeRequest()
        assertEquals("PUT", req.method)
        val body = req.body.readUtf8()
        assertTrue(body.contains(""""legal_name":"Tacos SA de CV""""), body)
        assertTrue(body.contains(""""address":{"street":"Reforma","ext_number":"10","neighborhood":"Juárez""""), body)
        assertTrue(!body.contains("curp") && !body.contains("int_number"), "absent optionals are omitted: $body")
    }

    @Test
    fun uploadIsMultipartWithProgress() {
        server.enqueue(MockResponse().setBody("""{"document_id":"doc_1","type":"INE_FRONT","status":"RECEIVED"}"""))
        val bytes = ByteArray(40_000) { 7 }
        val progress = mutableListOf<Long>()
        val r = api().uploadDocument("mer_1", "INE_FRONT", PickedDocument(bytes, "image/jpeg", "ine_front.jpg")) { sent, total ->
            assertEquals(40_000L, total)
            progress += sent
        } as ApiResult.Success
        assertEquals("doc_1", r.data.documentId)
        assertEquals(40_000L, progress.last())
        assertTrue(progress.size >= 3, "chunked progress: $progress")
        val req = server.takeRequest()
        assertEquals("/api/v1/merchants/mer_1/kyb/documents", req.path)
        assertTrue(req.getHeader("Content-Type")!!.startsWith("multipart/form-data"))
        val body = req.body.readUtf8()
        assertTrue(body.contains("name=\"type\"") && body.contains("INE_FRONT"))
        assertTrue(body.contains("name=\"file\"; filename=\"ine_front.jpg\""))
    }

    @Test
    fun submitPostsAndSurfacesErrors() {
        server.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"KYB_DOCUMENTS_INCOMPLETE"}"""))
        val r = api().submit("mer_1") as ApiResult.Error
        assertEquals("KYB_DOCUMENTS_INCOMPLETE", r.errorCode)
        assertEquals("/api/v1/merchants/mer_1/kyb/submit", server.takeRequest().path)
    }
}
