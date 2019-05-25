/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright(c) 2019 Wind River Systems, Inc. */

package common

import (
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud"
	perrors "github.com/pkg/errors"
	"github.com/wind-river/titanium-deployment-manager/pkg/apis/starlingx/v1beta1"
	"github.com/wind-river/titanium-deployment-manager/pkg/manager"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"net"
	"net/url"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"time"
)

var (
	// RetryImmediate should be used whenever a known transient error is
	// detected and there is a very likely that retrying immediately will
	// succeed.  For example,
	RetryImmediate = reconcile.Result{Requeue: true, RequeueAfter: time.Second}

	// RetrySystemNotReady should be used whenever a controller needs to wait
	// for the system controller to finish its reconcile task.  The system
	// controller kicks the other controllers when it has finish so there
	// is no need to automatically requeue these events.
	RetrySystemNotReady = reconcile.Result{Requeue: false}

	// RetryMissingClient should be used for any object reconciliation that
	// fails because of the platform client is missing or was reset.  The system
	// controller is responsible for re-creating the client and it will kick
	// the other controllers once it has re-established a connection to the
	// target system.
	RetryMissingClient = reconcile.Result{Requeue: false}

	// RetryTransientError should be used for any object reconciliation that
	// fails because of a transient error and needs to be re-attempted at a
	// future time.
	RetryTransientError = reconcile.Result{Requeue: true, RequeueAfter: 20 * time.Second}

	// RetryUserError should be used for any errors caught after an API request
	// that is likely due to data validation errors.  These could theoretically
	// not retry and just sit and wait for the user to correct the error, but
	// to mitigate against dependency errors or transient errors we will retry.
	RetryUserError = reconcile.Result{Requeue: true, RequeueAfter: time.Minute}

	// RetryValidationError should be used for any errors resulting from an
	// upfront validation error.  There is no point in trying again since the
	// data is invalid.  Just wait for the user to correct the issue.
	RetryValidationError = reconcile.Result{Requeue: false}

	// RetryServerError should be used for any errors caught after an API
	// request that is likely due to internal server errors.  These could
	// theoretically not retry and just sit and wait for the user to correct the
	// error, but to mitigate against dependency errors or transient errors we
	// will retry.
	RetryServerError = reconcile.Result{Requeue: true, RequeueAfter: time.Minute}

	// RetryNetworkError should be used for any DNS resolution errors.  There
	// is a good chance that these errors will persist for a while until the
	// user intervenes so slow down retry attempts.
	RetryResolutionError = reconcile.Result{Requeue: true, RequeueAfter: 5 * time.Minute}

	// RetryNetworkError should be used for any errors caught after a API
	// request that is likely due to network errors.  This could happen
	// because of a misconfiguration of the endpoint URL or whenever the system
	// becomes temporarily unreachable.  We need to retry until the system
	// becomes reachable.  Since the most likely explanation is that the
	// active controller was rebooted then it makes sense to keep retrying
	// frequently because it will come back relatively quickly.
	// TODO(alegacy): consider backing off using a rate limiter queue.
	RetryNetworkError = reconcile.Result{Requeue: true, RequeueAfter: 15 * time.Second}
)

// Common event record reasons
const (
	ResourceCreated    = "Created"
	ResourceUpdated    = "Updated"
	ResourceDeleted    = "Deleted"
	ResourceWait       = "Wait"
	ResourceDependency = "Dependency"
)

func FormatStruct(obj interface{}) string {
	buf, _ := json.Marshal(obj)
	return string(buf)
}

// ReconcilerErrorHandler defines the interface type associated to any
// reconciler error handler.
type ReconcilerErrorHandler interface {
	HandleReconcilerError(request reconcile.Request, in error) (reconcile.Result, error)
}

// ErrorHandler is the common implementation of the ReconcilerErrorHandler
// interface.
type ErrorHandler struct {
	logr.Logger
	manager.TitaniumManager
}

// HandleReconcilerError is the common error handler implementation for all
// controllers.  It is responsible for looking at the type of error that was
// caught and determine what the best resolution might be.
func (h *ErrorHandler) HandleReconcilerError(request reconcile.Request, in error) (result reconcile.Result, err error) {
	resetClient := true

	// We use wrapped errors throughout the system so make sure we are looking
	// at the initial error before determining what actually went wrong.
	cause := perrors.Cause(in)

	switch cause.(type) {
	case gophercloud.ErrDefault400, gophercloud.ErrDefault403,
		gophercloud.ErrDefault404, gophercloud.ErrDefault405:
		// These errors are resource based errors.  This means we successfully
		// submitted the request but the server rejected it therefore the client
		// is still valid.  There is likely a problem with the data provided
		// by the user so wait for the user to correct the data.  Retrying is
		// pointless
		resetClient = false
		result = RetryUserError
		err = nil

		h.Error(in, "user error during reconciliation", "request", request)

	case gophercloud.ErrDefault500, gophercloud.ErrDefault503:
		// These errors are server based errors.  This means we successfully
		// submitted the request but the server encountered an unexpected or
		// unhandled exception
		resetClient = false
		result = RetryServerError
		err = nil

		h.Error(in, "server error during reconciliation", "request", request)

	case *errors.StatusError:
		// These errors are rest client errors from client-go.
		resetClient = false
		err = nil

		if strings.Contains(cause.Error(), "object has been modified") {
			// This is likely a status update conflict so immediately retry.
			result = RetryImmediate
			h.Info("status update conflict during reconciliation", "request", request)
		} else {
			result = RetryTransientError
			h.Error(in, "status error during reconciliation", "request", request)
		}

	case *url.Error:
		// These errors are networking type errors.  We failed to reach or
		// connect to the server.  Reset the client in all cases
		urlError := cause.(*url.Error)

		resetClient = true
		result = RetryNetworkError
		err = nil

		if opError, ok := urlError.Err.(*net.OpError); ok {
			if _, ok := opError.Err.(*net.DNSError); ok {
				// For this specific error we know that more time will be
				// needed for the user to intervene so use a longer delay.
				result = RetryResolutionError
				h.Error(in, "resolution error during reconciliation", "request")
				break
			}

		} else if strings.Contains(urlError.Error(), manager.HTTPSNotEnabled) {
			h.Info("HTTPS request was sent to an non HTTPS system")

			// The system controller will need to deal with this error when
			// it attempts to rebuild the client.
		}

		h.Error(in, "URL error during reconciliation", "request", request)

	case HTTPSClientRequired:
		// These errors are generated when the system controllers discovers
		// that a requires that HTTPS be enabled first.
		resetClient = true
		result = RetryTransientError
		err = nil

		h.Error(in, "HTTPS client required", "request", request)

	case ValidationError, ChangeAfterInSync:
		// These errors are data validation errors.  There is likely a problem
		// with the data provided by the user so wait for the user to correct
		// the data.  Retrying is pointless.
		resetClient = false
		result = RetryValidationError
		err = nil

		h.Error(in, "validation error during reconciliation", "request")

	case ErrSystemDependency, ErrResourceStatusDependency:
		// These errors are transient errors.  Resources must be in stable
		// states before reconciling changes therefore we need to wait until
		// they settle before continuing.
		resetClient = false
		result = RetryTransientError
		err = nil

		h.Error(in, "resource status error during reconciliation", "request", request)

	case manager.Error, ErrUserDataError,
		v1beta1.ErrMissingSystemResource, ErrMissingKubernetesResource:
		// These errors are user data errors.  Usually a reference to a
		// non-existent resource.
		resetClient = false
		result = RetryUserError
		err = nil

		h.Error(in, "user data error during reconciliation", "request", request)

	default:
		resetClient = false

		if errors.IsNotFound(cause) == false {
			h.Error(in, "an unhandled error occurred", "type", reflect.TypeOf(cause))
			result = RetryTransientError
			err = in
		} else {
			// A request to the kubernetes client failed because of a missing
			// resource.  Assume that a user resource is not installed or
			// visible yet and try again.
			result = RetryUserError
			err = nil

			h.Error(in, "missing dependency during reconciliation", "request", request)
		}
	}

	if resetClient {
		if h.GetPlatformClient(request.Namespace) != nil {
			h.Info("resetting platform client")
			err2 := h.ResetPlatformClient(request.Namespace)
			if err2 != nil {
				h.Error(err2, "failed to reset platform client")
			}
		}
	}

	return result, err
}

// ReconcilerEventLogger is an interface that is intended to allow specialized
// behavior when generating an event.
type ReconcilerEventLogger interface {
	Event(object runtime.Object, eventtype string, reason string, messageFmt string, args ...interface{})
	NormalEvent(object runtime.Object, reason string, messageFmt string, args ...interface{})
	WarningEvent(object runtime.Object, reason string, messageFmt string, args ...interface{})
}

// EventLogger is an implementation of a ReconcilerEventLogger.  Its purpose is
// to simultaneously generate a log with every event and to prefix each event
// message with the object name.
type EventLogger struct {
	record.EventRecorder
	logr.Logger
}

// Event is a method used to generate a log and an event for a given set of
// message, event type, and reason.
func (in *EventLogger) Event(object runtime.Object, eventtype string, reason string, messageFmt string, args ...interface{}) {
	accessor := meta.NewAccessor()
	name, err := accessor.Name(object)
	if err != nil {
		name = "unknown"
	}
	msg := fmt.Sprintf("%s: %s", name, fmt.Sprintf(messageFmt, args...))
	in.Logger.Info(msg)
	in.EventRecorder.Eventf(object, eventtype, reason, msg)
}

// NormalEvent generates a log and event for a "normal" event.
func (in *EventLogger) NormalEvent(object runtime.Object, reason string, messageFmt string, args ...interface{}) {
	in.Event(object, v1.EventTypeNormal, reason, messageFmt, args...)
}

// WarningEvent generates a log and event for a "warning" event.
func (in *EventLogger) WarningEvent(object runtime.Object, reason string, messageFmt string, args ...interface{}) {
	in.Event(object, v1.EventTypeWarning, reason, messageFmt, args...)
}

func ContainsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func RemoveString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}