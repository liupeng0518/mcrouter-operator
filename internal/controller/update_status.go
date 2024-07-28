package controller

import (
	"context"
	"github.com/cloud/memcached-operator/api/v1alpha1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *MemcachedReconciler) UpdateMemcached(ctx context.Context, memcached *v1alpha1.Memcached, newMem v1alpha1.MemcachedStatus) error {
	// 采用 retry 进行默认的 backoff 重试，如果 update 失败则再次获取新版本再更新
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		if err = r.Client.Get(ctx, client.ObjectKey{Name: memcached.Name, Namespace: memcached.Namespace}, memcached); err != nil {
			return
		}

		// 再次更新 foo.Spec.Foo 字段
		memcached.Status = newMem
		return r.Update(ctx, memcached)
	})
}
