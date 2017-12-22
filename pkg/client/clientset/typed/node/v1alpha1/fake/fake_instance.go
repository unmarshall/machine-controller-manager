package fake

import (
	v1alpha1 "code.sapcloud.io/kubernetes/node-controller-manager/pkg/apis/node/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeInstances implements InstanceInterface
type FakeInstances struct {
	Fake *FakeNodeV1alpha1
}

var instancesResource = schema.GroupVersionResource{Group: "node.sapcloud.io", Version: "v1alpha1", Resource: "instances"}

var instancesKind = schema.GroupVersionKind{Group: "node.sapcloud.io", Version: "v1alpha1", Kind: "Instance"}

// Get takes name of the instance, and returns the corresponding instance object, and an error if there is any.
func (c *FakeInstances) Get(name string, options v1.GetOptions) (result *v1alpha1.Instance, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootGetAction(instancesResource, name), &v1alpha1.Instance{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Instance), err
}

// List takes label and field selectors, and returns the list of Instances that match those selectors.
func (c *FakeInstances) List(opts v1.ListOptions) (result *v1alpha1.InstanceList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootListAction(instancesResource, instancesKind, opts), &v1alpha1.InstanceList{})
	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1alpha1.InstanceList{}
	for _, item := range obj.(*v1alpha1.InstanceList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested instances.
func (c *FakeInstances) Watch(opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewRootWatchAction(instancesResource, opts))
}

// Create takes the representation of a instance and creates it.  Returns the server's representation of the instance, and an error, if there is any.
func (c *FakeInstances) Create(instance *v1alpha1.Instance) (result *v1alpha1.Instance, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootCreateAction(instancesResource, instance), &v1alpha1.Instance{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Instance), err
}

// Update takes the representation of a instance and updates it. Returns the server's representation of the instance, and an error, if there is any.
func (c *FakeInstances) Update(instance *v1alpha1.Instance) (result *v1alpha1.Instance, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootUpdateAction(instancesResource, instance), &v1alpha1.Instance{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Instance), err
}

// Delete takes name of the instance and deletes it. Returns an error if one occurs.
func (c *FakeInstances) Delete(name string, options *v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewRootDeleteAction(instancesResource, name), &v1alpha1.Instance{})
	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeInstances) DeleteCollection(options *v1.DeleteOptions, listOptions v1.ListOptions) error {
	action := testing.NewRootDeleteCollectionAction(instancesResource, listOptions)

	_, err := c.Fake.Invokes(action, &v1alpha1.InstanceList{})
	return err
}

// Patch applies the patch and returns the patched instance.
func (c *FakeInstances) Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *v1alpha1.Instance, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceAction(instancesResource, name, data, subresources...), &v1alpha1.Instance{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Instance), err
}