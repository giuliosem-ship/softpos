# Official SAT XSLT (vendored for golden tests)

Unmodified copies of the SAT stylesheets that define the CFDI 4.0 cadena
original (Anexo 20). `cadena_xslt_test.go` runs them with `xsltproc`
against XML produced by `BuildCFDI40XML` and compares the result with the Go
builder `BuildCadenaOriginal`.

| File | Source URL | Downloaded | Last-Modified (server) | SHA-256 |
|---|---|---|---|---|
| `cadenaoriginal_4_0.xslt` | http://www.sat.gob.mx/sitio_internet/cfd/4/cadenaoriginal_4_0/cadenaoriginal_4_0.xslt | 2026-11-20 | 2026-04-17 | `b0559b380e73b850ca8a3da53b077a6051a09d87068d8d98e82dfd4acfba7565` |
| `utilerias.xslt` | http://www.sat.gob.mx/sitio_internet/cfd/2/cadenaoriginal_2_0/utilerias.xslt | 2026-11-20 | — | `1f7e9c9c04b2239cdf0928e6471d4f48165535c4d4d3ea445ec310b65cdf6c2c` |

The files are kept byte-identical. At test time a copy of
`cadenaoriginal_4_0.xslt` is written to a temporary directory with the
`utilerias.xslt` include pointed at the local file and the complement
includes (nómina, pagos, carta porte, …) removed, so `xsltproc --nonet` runs
offline. Kerno emits no complements in the comprobante (the
TimbreFiscalDigital has its own cadena), so those templates are not needed.

To refresh: download both URLs again, update this table and run
`go test ./pkg/fiscal -run XSLT -v`.
