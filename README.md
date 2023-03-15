# portforward-helper

Port forward helper for ArangoGraph Insights Platform.

Provides the implementation of HTTP streaming to allow port-forwarding
from firewalled server to the Platform.

### Credits

The implementation is based on Kubernetes project source code, specifically [k8s.io/apimachinery/pkg/util/httpstream](https://pkg.go.dev/k8s.io/apimachinery/pkg/util/httpstream) package.
Main changes are:
- support for port forwarding in reverse direction
- all k8s apimachinery and dependencies removed
- no klog logging - the logging is delegated to client of this package