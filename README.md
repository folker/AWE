![AWE](docs/images/awe-lg.png)
=====

About:
------

AWE (Argonne Workflow Engine) is a workload management system for bioinformatic workflow applications. AWE, together with Shock data management system, can be used to build an integrated platform for efficient data analysis and management which features following functionalities:

- Common workflow language support
- Multi cloud support
- Explicit task parallelization and convenient application integration
- Scalable, portable, and fault-tolerant workflow computation
- Integration of heterogeneous and geographically distributed computing resources
- Performance-aware, cost-efficient service management and resource management
- Reusable and reproducible data product management 

![awe-diagram](docs/images/awe-diagram.png)

AWE is designed as a distributed system that contains a centralized server and multiple distributed clients. The server receives job submissions and parses jobs into tasks, splits tasks into workunits, and manages workunits in a queue. The AWE clients, running on distributed, heterogeneous computing resources, keep checking out workunits from the server queue and dispatching the workunits on the local computing resources. 

AWE uses the Shock data management system to handle input and output data (retrieval, storage, splitting, and merge). AWE uses a RESTful API for communication between AWE components and with outside components such as Shock, the job submitter, and the status monitor.

![awe-diagram](docs/images/awe-multi-site.png)


Related Links
------
| repository | description    | link |
| ----------- | ----------- | ----------- |
| AWE monitor | UI for the AWE server | [github.com/MG-RAST/awe-monitor](https://github.com/MG-RAST/awe-monitor) |
| Shock       | object store | [github.com/MG-RAST/Shock](https://github.com/MG-RAST/Shock) |
| Skyport2    | demo environment using docker-compose | [github.com/MG-RAST/Skyport2](https://github.com/MG-RAST/Skyport2) |


Documentation
------
Documentation can be found on the AWE github pages:

https://mg-rast.github.io/AWE/



Papers to cite
------

W. Tang, J. Wilkening, N. Desai, W. Gerlach, A. Wilke, F. Meyer, "A scalable data analysis platform for metagenomics," in Proc. of IEEE International Conference on Big Data, 2013.[[ieeexplore]](http://ieeexplore.ieee.org/xpl/articleDetails.jsp?arnumber=6691723) [[pdf]](http://www.mcs.anl.gov/papers/P5012-0913_1.pdf)

W. Gerlach, W. Tang, K. Keegan, T. Harrison, A. Wilke, J. Bischof, M. D'Souza, S. Devoid, D. Murphy-Olson, N. Desai, F. Meyer, "Skyport – Container-Based Execution Environment Management for Multi-Cloud Scientific Workflows," in Proc. of the 5th International Workshop on Data Intensive Computing in the Clouds, 2014. [[pdf]](https://www.mcs.anl.gov/papers/P5209-1014.pdf)


