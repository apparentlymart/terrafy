# Terrafy

Terrafy is a **highly experimental** utility for batch-importing a number of
remote objects into a Terraform state and then generating an associated
configuration for them.

I wrote it primarily to explore the problem space of implementing these
features generically using the current provider featureset. It might end up
being somehow useful for real-world importing, but I make no guarantees about
the quality of its results or its stability. (If you're importing into an
entirely new, "greenfield" Terraform configuration then at least you could just
discard the result if it's not acceptable.)

Please note that this is not a HashiCorp project and I don't intend to spend
any time supporting or maintaining it once it has served its research purpose.

## How it works

Terrafy has its own language which draws some of the concepts from the
Terraform language but also has some additional features for the problem of
importing.

The intended usage pattern is to first write a normal Terraform root module that
contains the necessary provider requirement declarations (a
`required_providers` block) and any necessary provider configurations.
Then, we can write one or more Terrafy-specific `.tfy` files that declare what
is to be imported.

With both the `.tf` files defining the providers and the `.tfy` files defining
the import goals, we can run `terrafy` to try to run the import:

```
$ terrafy
Import plan:
- Create Terraform state binding from aws_instance.example[0] to remote object "i-abc123"
- Create Terraform state binding from aws_instance.example[1] to remote object "i-def456"
- Create Terraform state binding from aws_instance.example[2] to remote object "i-ghi789"
- Generate a new aws_instance.example configuration block in main.tf

Do you want to proceed? (Only "yes" will be accepted to confirm.)
> yes

Importing:
- terraform import 'aws_instance.example[0]' 'i-abc123'
- terraform import 'aws_instance.example[1]' 'i-def456'
- terraform import 'aws_instance.example[2]' 'i-ghi789'
- fetching the latest Terraform state snapshot
- adding a new resource "aws_instance" "example" block to main.tf

All done! Confirm the result by trying to create a Terraform plan:
    terraform plan
```

## The Terrafy Language

The following is an example `main.tfy` file that might generate a session
similar to the one shown in the above example:

```hcl
data "aws_instances" "existing" {
  instance_tags = {
    Subsystem   = "renderer"
    Environment = "production"
  }
}

import "aws_instance" "example" {
  id = tolist(data.aws_instances.existing.ids)
}
```

An `data` block declares a data resource in the same way as the equivalent
construct in the main Terraform language.

An `import` block declares the goal of importing one or more remote objects
into a new managed resource. `import "aws_instance" "example"` represents
the intent to create `resource "aws_instance" "example"`.

The `id` argument in an `import` block is either a single string, a list
of strings, or a map of strings. The type decides whether the resulting
resource will have a single instance, whether it will use `count`, or whether
it will use `for_each` respectively.

In the above example, we used the `aws_instances` data source to find a set
of EC2 instances tagged in a particular way, and then passed their ids
dynamically to be the ids for the imported `aws_instance.example` instances.

As with `.tf` files, you can have many `.tfy` files in your root module
directory. Terrafy uses the basename of the `.tfy` file to decide which
`.tf` file the resulting `resource` blocks should be generated into. In the
above example, because the `import` block is in a file called `main.tfy`
Terrafy will generate a `resource "aws_instance" "example"` block in the
`main.tf` file, creating it if necessary.

## How Terrafy works

Terrafy is an experimental prototype, so it has
[some quirky behaviors](./docs/quirks.md) and its implementation as a tool
outside of Terraform (as oppposed to a built-in Terraform feature) means it
has to use some tricky techniques to get its work done.

The main thing to understand about Terrafy is that it implements its own
language by translating it to a temporary configuration written in the
Terraform language and then running `terraform apply` against it. The
example in the previous section might be lowered into a Terraform configuration
like this, for example:

```hcl
data "aws_instances" "existing" {
  instance_tags = {
    Subsystem   = "renderer"
    Environment = "production"
  }
}

output "ids" {
  value = {
    "aws_instance.example" = tolist(data.aws_instances.existing.ids)
  }
}
```

Notice that all of the `data` blocks are copied in verbatim, but the `import`
blocks are all combined together into a single `ids` output which is a table
of all of the generated ids.

After applying this generated configuration, Terrafy then reads the `ids`
output value from the state and uses it to produce the import plan.

Because Terrafy is generating and applying a temporary Terraform configuration
behind the scenes, the underlying details will tend to leak into its UI
when something goes wrong. For example, if the data resource above were to
fail with an error message, the error you see will be in terms of the temporary
generated configuration rather than your original `.tfy` file.

## This really is Experimental!

This tool is not well-tested and will probably crash when encountering cases
I didn't think to test yet.

I would strongly recommend against using it with any Terraform state or
configuration you care about. Playing with an entirely new configuration
and state could be fine though, because you can always discard that state
and configuration if something doesn't work out.
