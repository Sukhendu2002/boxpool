# M3 macOS pause/resume benchmark

## Result

Run on 2026-10-28 on an Apple M2 with 16 GB RAM, macOS 27.0.1, arm64. Three integration runs completed successfully:

| Run | Pause | Resume until agent answered |
| --- | ---: | ---: |
| 1 | 399 ms | 223 ms |
| 2 | 449 ms | 222 ms |
| 3 | 406 ms | 230 ms |

The observed resume time is below the M3 gate of 1 s. The integration test also verified that a background guest process continued writing across pause, daemon restart, and resume.

## Reproduce

```sh
go test -c -tags integration -o /tmp/vz.test ./internal/backend/vz
scripts/dev-sign.sh /tmp/vz.test
/tmp/vz.test -test.v -test.timeout 5m -test.run '^TestIntegrationPause$' -test.count=3
```
