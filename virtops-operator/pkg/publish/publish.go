package publish

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Package publish handles the optional publishing of rotated credentials
// into Kubernetes Secrets according to the policy: Always, Never.

type Mode string

const (
	ModeAlways Mode = "Always"
	ModeNever  Mode = "Never"
)

func Do(ctx context.Context, c client.Client, namespace string, mode Mode, secretName string, labels map[string]string, data map[string][]byte) error {
	if mode == "" || mode == ModeNever {
		return nil
	}
	if mode != ModeAlways {
		return fmt.Errorf("unsupported publish mode %q", mode)
	}
	if c == nil {
		return fmt.Errorf("k8s client is nil")
	}
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if secretName == "" {
		return fmt.Errorf("secretName is required")
	}
	if len(data) == 0 {
		return fmt.Errorf("no data to publish")
	}

	var lastErr error
	backoff := 100 * time.Millisecond
	for attempt := 0; attempt < 6; attempt++ {
		var s corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &s)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}

			ns := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: namespace,
					Labels:    map[string]string{},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{},
			}
			for k, v := range labels {
				ns.Labels[k] = v
			}
			for k, v := range data {
				ns.Data[k] = v
			}
			err = c.Create(ctx, ns)
			if err == nil {
				return nil
			}
			if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
				lastErr = err
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			return err
		}

		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		for k, v := range labels {
			s.Labels[k] = v
		}
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		for k, v := range data {
			s.Data[k] = v
		}

		err = c.Update(ctx, &s)
		if err == nil {
			return nil
		}
		if apierrors.IsConflict(err) {
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		return err
	}

	return fmt.Errorf("publish secret %s/%s conflict: %w", namespace, secretName, lastErr)
}
