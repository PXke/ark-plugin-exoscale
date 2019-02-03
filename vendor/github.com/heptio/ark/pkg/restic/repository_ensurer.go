/*
Copyright 2018 the Heptio Ark contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package restic

import (
	"context"
	"sync"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	arkv1api "github.com/heptio/ark/pkg/apis/ark/v1"
	arkv1client "github.com/heptio/ark/pkg/generated/clientset/versioned/typed/ark/v1"
	arkv1informers "github.com/heptio/ark/pkg/generated/informers/externalversions/ark/v1"
	arkv1listers "github.com/heptio/ark/pkg/generated/listers/ark/v1"
)

// repositoryEnsurer ensures that Ark restic repositories are created and ready.
type repositoryEnsurer struct {
	repoLister arkv1listers.ResticRepositoryLister
	repoClient arkv1client.ResticRepositoriesGetter

	readyChansLock sync.Mutex
	readyChans     map[string]chan *arkv1api.ResticRepository
}

func newRepositoryEnsurer(repoInformer arkv1informers.ResticRepositoryInformer, repoClient arkv1client.ResticRepositoriesGetter, log logrus.FieldLogger) *repositoryEnsurer {
	r := &repositoryEnsurer{
		repoLister: repoInformer.Lister(),
		repoClient: repoClient,
		readyChans: make(map[string]chan *arkv1api.ResticRepository),
	}

	repoInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, upd interface{}) {
				oldObj := old.(*arkv1api.ResticRepository)
				newObj := upd.(*arkv1api.ResticRepository)

				if oldObj.Status.Phase != arkv1api.ResticRepositoryPhaseReady && newObj.Status.Phase == arkv1api.ResticRepositoryPhaseReady {
					r.readyChansLock.Lock()
					defer r.readyChansLock.Unlock()

					readyChan, ok := r.readyChans[newObj.Name]
					if !ok {
						log.Errorf("No ready channel found for repository %s/%s", newObj.Namespace, newObj.Name)
						return
					}

					readyChan <- newObj
					delete(r.readyChans, newObj.Name)
				}
			},
		},
	)

	return r
}

func (r *repositoryEnsurer) EnsureRepo(ctx context.Context, namespace, name string) (*arkv1api.ResticRepository, error) {
	if repo, err := r.repoLister.ResticRepositories(namespace).Get(name); err != nil && !apierrors.IsNotFound(err) {
		return nil, errors.WithStack(err)
	} else if err == nil {
		if repo.Status.Phase != arkv1api.ResticRepositoryPhaseReady {
			return nil, errors.New("restic repository is not ready")
		}
		return repo, nil
	}

	// if we're here, it means we got an IsNotFound error, meaning we need to create a new
	// repo and wait for it to be ready

	repo := &arkv1api.ResticRepository{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: arkv1api.ResticRepositorySpec{
			MaintenanceFrequency: metav1.Duration{Duration: DefaultMaintenanceFrequency},
		},
	}

	readyChan := r.getReadyChan(name)
	defer close(readyChan)

	if _, err := r.repoClient.ResticRepositories(namespace).Create(repo); err != nil {
		return nil, errors.Wrapf(err, "unable to create restic repository resource")
	}

	select {
	case <-ctx.Done():
		return nil, errors.New("timed out waiting for restic repository to become ready")
	case res := <-readyChan:
		return res, nil
	}
}

func (r *repositoryEnsurer) getReadyChan(name string) chan *arkv1api.ResticRepository {
	r.readyChansLock.Lock()
	defer r.readyChansLock.Unlock()

	r.readyChans[name] = make(chan *arkv1api.ResticRepository)
	return r.readyChans[name]
}
